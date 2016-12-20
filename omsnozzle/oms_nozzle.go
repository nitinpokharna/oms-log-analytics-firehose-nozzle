package omsnozzle

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cloudfoundry-community/go-cfclient"
	"github.com/cloudfoundry-incubator/uaago"
	"github.com/cloudfoundry/noaa/consumer"
	events "github.com/cloudfoundry/sonde-go/events"
	"github.com/lizzha/pcf-oms-poc/client"
	"github.com/lizzha/pcf-oms-poc/messages"
)

const (
	maxPostGoroutines = 1000
	// Max message size of a sigle post
	maxSizePerBatch = 20000000
)

type OmsNozzle struct {
	errChan            <-chan error
	msgChan            <-chan *events.Envelope
	signalChan         chan os.Signal
	consumer           *consumer.Consumer
	omsClient          *client.Client
	cfClientConfig     *cfclient.Config
	nozzleConfig       *NozzleConfig
	nozzleInstanceName string
	goroutineSem       chan int // to control the number of active post goroutines
}

type NozzleConfig struct {
	UaaAddress             string
	UaaClientName          string
	UaaClientSecret        string
	TrafficControllerUrl   string
	SkipSslValidation      bool
	IdleTimeout            time.Duration
	FirehoseSubscriptionId string
	OmsTypePrefix          string
	OmsBatchTime           time.Duration
	ExcludeMetricEvents    bool
	ExcludeLogEvents       bool
	ExcludeHttpEvents      bool
}

func NewOmsNozzle(cfClientConfig *cfclient.Config, omsClient *client.Client, nozzleConfig *NozzleConfig) *OmsNozzle {
	return &OmsNozzle{
		errChan:        make(<-chan error),
		msgChan:        make(<-chan *events.Envelope),
		signalChan:     make(chan os.Signal, 1),
		omsClient:      omsClient,
		cfClientConfig: cfClientConfig,
		nozzleConfig:   nozzleConfig,
		goroutineSem:   make(chan int, maxPostGoroutines),
	}
}

func (o *OmsNozzle) Start() error {
	o.initialize()
	err := o.routeEvents()
	return err
}

func (o *OmsNozzle) setInstanceName() error {
	// instance id to track multiple nozzles, used for logging
	hostName, err := os.Hostname()
	if err != nil {
		fmt.Printf("Error getting hostname for NozzleInstance: %s\n", err)
		o.nozzleInstanceName = fmt.Sprintf("pid-%d", os.Getpid())
	} else {
		o.nozzleInstanceName = fmt.Sprintf("pid-%d@%s", os.Getpid(), hostName)
	}
	fmt.Printf("Nozzle instance name: %s\n", o.nozzleInstanceName)
	return err
}

func (o *OmsNozzle) initialize() {
	// setup for termination signal from CF
	signal.Notify(o.signalChan, syscall.SIGTERM, syscall.SIGINT)
	o.setInstanceName()

	fmt.Printf("Starting with uaaAddress:%s, dopplerAddress:%s\n", o.nozzleConfig.UaaAddress, o.nozzleConfig.TrafficControllerUrl)
	uaaClient, err := uaago.NewClient(o.nozzleConfig.UaaAddress)
	if err != nil {
		panic("Error creating uaa client:" + err.Error())
	}
	authToken, err := uaaClient.GetAuthToken(o.nozzleConfig.UaaClientName, o.nozzleConfig.UaaClientSecret, o.nozzleConfig.SkipSslValidation)
	if err != nil {
		panic("Error getting auth token:" + err.Error())
	}

	o.consumer = consumer.New(
		o.nozzleConfig.TrafficControllerUrl,
		&tls.Config{InsecureSkipVerify: o.nozzleConfig.SkipSslValidation},
		nil)

	refresher := tokenRefresher{
		uaaClient:           uaaClient,
		clientName:          o.nozzleConfig.UaaClientName,
		clientSecret:        o.nozzleConfig.UaaClientSecret,
		skipSslVerification: o.nozzleConfig.SkipSslValidation,
	}
	o.consumer.RefreshTokenFrom(&refresher)
	o.consumer.SetIdleTimeout(o.nozzleConfig.IdleTimeout)
	o.msgChan, o.errChan = o.consumer.Firehose(o.nozzleConfig.FirehoseSubscriptionId, authToken)

	//o.consumer.SetDebugPrinter(ConsoleDebugPrinter{})
	// async error channel
	go func() {
		var errorChannelCount = 0
		for err := range o.errChan {
			errorChannelCount++
			fmt.Fprintf(os.Stderr, "Firehose channel error.  Date:%v errorCount:%d error:%v\n", time.Now(), errorChannelCount, err.Error())
		}
	}()
}

func (o *OmsNozzle) postData(events *map[string][]byte) {
	for k, v := range *events {
		if len(v) > 0 {
			v = append(v, ']')
			if len(o.nozzleConfig.OmsTypePrefix) > 0 {
				k = o.nozzleConfig.OmsTypePrefix + k
			}
			nRetries := 4
			for nRetries > 0 {
				requestStartTime := time.Now()
				if err := o.omsClient.PostData(&v, k); err != nil {
					nRetries--
					elapsedTime := time.Since(requestStartTime)
					fmt.Printf("Error posting message type %s to OMS. error: %s elapseTime:%s msgSize:%d. remaining attempts=%d\n", k, err, elapsedTime.String(), len(v), nRetries)
					time.Sleep(time.Second * 1)
				} else {
					break
				}
			}
		}
	}
	<-o.goroutineSem
}

func marshalAsJson(m *OMSMessage) []byte {
	if j, err := json.Marshal(&m); err != nil {
		fmt.Printf("Error marshalling message to JSON. error: %s. message: %s\n", err, *m)
		return nil
	} else {
		return j
	}
}

func pushMsgAsJson(eventType string, events *map[string][]byte, msg *[]byte) {
	// Push json messages to json array format
	if len((*events)[eventType]) == 0 {
		(*events)[eventType] = append((*events)[eventType], '[')
	} else {
		(*events)[eventType] = append((*events)[eventType], ',')
	}
	(*events)[eventType] = append((*events)[eventType], *msg...)
}

func (o *OmsNozzle) routeEvents() error {
	pendingEvents := make(map[string][]byte)
	// Firehose message processing loop
	ticker := time.NewTicker(o.nozzleConfig.OmsBatchTime)
	for {
		// loop over message and signal channel
		select {
		case s := <-o.signalChan:
			fmt.Printf("Signal caught:%s Exiting\n", s.String())
			err := o.consumer.Close()
			if err != nil {
				fmt.Printf("Error closing consumer:%v\n", err)
			}
			os.Exit(1)
		case <-ticker.C:
			// get the pending as current
			currentEvents := pendingEvents
			// reset the pending events
			pendingEvents = make(map[string][]byte)
			o.goroutineSem <- 1
			go o.postData(&currentEvents)
		case msg := <-o.msgChan:
			// process message
			var omsMessage OMSMessage
			var omsMessageType = msg.GetEventType().String()
			switch msg.GetEventType() {
			// Metrics
			case events.Envelope_ValueMetric:
				if !o.nozzleConfig.ExcludeMetricEvents {
					omsMessage = messages.NewValueMetric(msg, o.nozzleInstanceName)
					if m := marshalAsJson(&omsMessage); m != nil {
						pushMsgAsJson(omsMessageType, &pendingEvents, &m)
					}
				}
			case events.Envelope_CounterEvent:
				if !o.nozzleConfig.ExcludeMetricEvents {
					omsMessage = messages.NewCounterEvent(msg, o.nozzleInstanceName)
					if m := marshalAsJson(&omsMessage); m != nil {
						pushMsgAsJson(omsMessageType, &pendingEvents, &m)
					}
				}

			case events.Envelope_ContainerMetric:
				if !o.nozzleConfig.ExcludeMetricEvents {
					omsMessage = messages.NewContainerMetric(msg, o.nozzleInstanceName)
					if m := marshalAsJson(&omsMessage); m != nil {
						pushMsgAsJson(omsMessageType, &pendingEvents, &m)
					}
				}

			// Logs Errors
			case events.Envelope_LogMessage:
				if !o.nozzleConfig.ExcludeLogEvents {
					omsMessage = messages.NewLogMessage(msg, o.nozzleInstanceName)
					if m := marshalAsJson(&omsMessage); m != nil {
						pushMsgAsJson(omsMessageType, &pendingEvents, &m)
					}
				}

			case events.Envelope_Error:
				if !o.nozzleConfig.ExcludeLogEvents {
					omsMessage = messages.NewError(msg, o.nozzleInstanceName)
					if m := marshalAsJson(&omsMessage); m != nil {
						pushMsgAsJson(omsMessageType, &pendingEvents, &m)
					}
				}

			// HTTP Start/Stop
			case events.Envelope_HttpStartStop:
				if !o.nozzleConfig.ExcludeHttpEvents {
					omsMessage = messages.NewHTTPStartStop(msg, o.nozzleInstanceName)
					if m := marshalAsJson(&omsMessage); m != nil {
						pushMsgAsJson(omsMessageType, &pendingEvents, &m)
					}
				}
			default:
				fmt.Println("Unexpected message type" + msg.GetEventType().String())
				continue
			}
			// When the size of one type reaches the max per batch, trigger the post immediately
			doPost := false
			for _, v := range pendingEvents {
				if len(v) >= maxSizePerBatch {
					doPost = true
					break
				}
			}
			if doPost {
				currentEvents := pendingEvents
				pendingEvents = make(map[string][]byte)
				o.goroutineSem <- 1
				go o.postData(&currentEvents)
			}
		default:
		}
	}
}

// OMSMessage is a marker inteface for JSON formatted messages published to OMS
type OMSMessage interface{}

// ConsoleDebugPrinter for debug logging
type ConsoleDebugPrinter struct{}

// Print debug logging
func (c ConsoleDebugPrinter) Print(title, dump string) {
	fmt.Printf("Consumer debug.  title:%s detail:%s", title, dump)
}

type tokenRefresher struct {
	uaaClient           *uaago.Client
	clientName          string
	clientSecret        string
	skipSslVerification bool
}

func (t *tokenRefresher) RefreshAuthToken() (string, error) {
	token, err := t.uaaClient.GetAuthToken(t.clientName, t.clientSecret, t.skipSslVerification)
	if err != nil {
		return "", err
	}
	return token, nil
}
