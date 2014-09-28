package main

import (
    "os"
    "os/signal"
    "syscall"
    "fmt"
    "time"
    
    log "github.com/Sirupsen/logrus"
    flags "github.com/jessevdk/go-flags"

    "github.com/armon/consul-api"
    "github.com/amir/raidman"
)

type Options struct {
    Debug          bool   `          long:"debug"                                            description:"enable debug logging"`
    LogFile        string `short:"l" long:"log-file"                                         description:"JSON log file path"`
    RiemannHost    string `          long:"riemann-host" required:"true"                     description:"Riemann host"`
    RiemannPort    int    `          long:"riemann-port"                 default:"5555"      description:"Riemann port"`
    Proto          string `          long:"proto"                        default:"udp"       description:"protocol to use when sending Riemann events"`
    ConsulHost     string `          long:"consul-host"                  default:"127.0.0.1" description:"Consul host"`
    ConsulPort     int    `          long:"consul-port"                  default:"8500"      description:"Consul port"`
    UpdateInterval string `          long:"interval"                     default:"1m"        description:"how frequently to post events to Riemann"`
    LockDelay      string `          long:"lock-delay"                   default:"15s"       description:"lock delay after session invalidation"`
}

func sendHealthResults(riemann RiemannClient, healthResults []consulapi.HealthCheck, updateInterval time.Duration) error {
    for _, healthCheck := range healthResults {
        // {
        //   "Name": "Service 'client-youngaustria' check",
        //   "ServiceName": "client-youngaustria",
        //   "Node": "web-fwork-gen-028.us-east-1.aws.prod.bsdinternal.com"
        //   "CheckID": "service:client-youngaustria",
        //   "ServiceID": "client-youngaustria",
        //   "Output": "TTL expired",
        //   "Notes": "",
        //   "Status": "critical",
        // },

        // Riemann event TTL: A floating-point time, in seconds, that
        // this event is considered valid for
        eventTtl := float32((updateInterval * 3) / time.Second)
        
        // convert Consul status to Riemann state
        state := map[string]string{
            "passing":  "ok",
            "warning":  "warning",
            "critical": "critical",
        }[healthCheck.Status]
        
        evt := &raidman.Event{
            Ttl:         eventTtl,
            Time:        time.Now().Unix(),
            Tags:        append([]string{ "consul" }),
            Host:        healthCheck.Node,
            State:       state,
            Service:     healthCheck.CheckID, // @todo CheckID or ServiceID?
            Description: healthCheck.Output,
        }
        
        err := riemann.Send(evt)
        
        if err != nil {
            return err
        }
    }
    
    return nil
}

func mainLoop(receiver *ConsulReceiver, riemann RiemannClient, updateInterval time.Duration) {
    // used to notify when lock has been lost; it'll just get closed
    var lockWatchChan chan interface{}
    
    // receives HealthCheck results
    var healthResultsChan chan []consulapi.HealthCheck

    keepGoing := true
    haveLock := false
    
    for keepGoing {
        // @todo update health check only when don't have lock or when health
        // results are processed successfully.
        err := receiver.UpdateHealthCheck()
        checkError("unable to submit health check", err)

        if ! haveLock {
            log.Debug("acquiring lock")
            
            // don't have lock; attempt to acquire it. AcquireLock() blocks.
            haveLock, err = receiver.AcquireLock()
            checkError("error acquiring lock", err)
            
            if haveLock {
                log.Info("acquired lock")
                
                // get notified when we lose our lock
                lockWatchChan = make(chan interface{})
                go receiver.WatchLock(lockWatchChan)
                
                // start retrieving health results
                healthResultsChan = make(chan []consulapi.HealthCheck)
                go receiver.WatchHealthResults(healthResultsChan)
            } else {
                log.Debug("could not acquire lock")
            }
        }
        
        if haveLock {
            // AcquireLock blocks for the updateInterval period.  we only have
            // channels to read from if we've got the lock.
            
            select {
                // wait for the lock to be lost
                case <-lockWatchChan:
                    log.Warn("lost lock")
                    
                    haveLock = false
                    
                    // this could take some time
                    healthResultsChan <- nil
                    healthResultsChan = nil
                    log.Debug("closed health results channel")
                    
                    lockWatchChan = nil
                
                case healthResults, more := <-healthResultsChan:
                    // channel closed if there was an error retrieving the health
                    // results. also check that we still have the lock, as the
                    // health results channel could still have results.
                    log.Debug("got health results")

                    if more && haveLock {
                        log.Debug("processing health results")
                        err := sendHealthResults(riemann, healthResults, updateInterval)
                        
                        if err != nil {
                            log.Errorf("error sending event to Riemann: %v", err)
                            
                            receiver.ReleaseLock()
                        }
                    } else {
                        // lost lock or error occurred retrieving health results
                        receiver.ReleaseLock()
                    }

                case <-time.After(updateInterval):
                    // timeout
            }
        }
    }
}

func main() {
    var opts Options
    // var riemann RiemannClient
    
    _, err := flags.Parse(&opts)
    if err != nil {
        os.Exit(1)
    }
    
    // parse UpdateInterval and LockDelay before setting up logging
    updateInterval, err := time.ParseDuration(opts.UpdateInterval)
    checkError(fmt.Sprintf("invalid update interval %s", opts.UpdateInterval), err)
    
    lockDelay, err := time.ParseDuration(opts.LockDelay)
    checkError(fmt.Sprintf("invalid lock delay %s", opts.LockDelay), err)
    
    if opts.Debug {
        // Only log the warning severity or above.
        log.SetLevel(log.DebugLevel)
    }
    
    if opts.LogFile != "" {
        logFp, err := os.OpenFile(opts.LogFile, os.O_WRONLY | os.O_APPEND | os.O_CREATE, 0600)
        checkError(fmt.Sprintf("error opening %s", opts.LogFile), err)
        
        defer logFp.Close()
        
        // log as JSON
        log.SetFormatter(&log.JSONFormatter{})
        
        // send output to file
        log.SetOutput(logFp)
    }
    
    // connect to Consul
    consulConfig := consulapi.DefaultConfig()
    consulConfig.Address = fmt.Sprintf("%s:%d", opts.ConsulHost, opts.ConsulPort)
    log.Infof("connecting to Consul at %s", consulConfig.Address)

    consul, err := consulapi.NewClient(consulConfig)
    checkError("unable to create consul client", err)
    
    receiver, err := NewConsulReceiver(
        consul.Agent(),
        consul.Session(),
        consul.KV(),
        consul.Health(),
        updateInterval,
        lockDelay,
        "riemann-consul-receiver",
        "services/riemann-consul-receiver",
    )
    
    checkError("unable to initialize consul receiver", err)
    
    err = receiver.RegisterService()
    checkError("unable to register service", err)
    
    _, err = receiver.InitSession()
    checkError("unable to init session", err)
    
    // destroy the session when the process exits
    defer receiver.DestroySession()
    
    // connect to Riemann
    riemannAddr := fmt.Sprintf("%s:%d", opts.RiemannHost, opts.RiemannPort)
    log.Infof("connecting to Riemann at %s", riemannAddr)

    riemann, err := raidman.Dial(opts.Proto, riemannAddr)
    checkError("unable to create riemann client", err)
    
    // receive OS signals so we can cleanly shut down
    // use syscall signals because os only provides Interrupt and Kill
    signalChan := make(chan os.Signal)
    signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)
    
    log.Debug("starting main loop")
    go mainLoop(receiver, riemann, updateInterval)
    
    // Block until a signal is received.
    <-signalChan
}