package events

import (
	"bytes"
	"encoding/base64"
	"encoding/json"

	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/gorilla/websocket"
	"github.com/rancher/go-rancher/client"
)

const MaxWait = time.Duration(time.Second * 10)

// EventHandler Defines the function "interface" that handlers must conform to.
type EventHandler func(*Event, *client.RancherClient) error

type EventRouter struct {
	name          string
	priority      int
	apiURL        string
	accessKey     string
	secretKey     string
	apiClient     *client.RancherClient
	subscribeURL  string
	eventHandlers map[string]EventHandler
	workerCount   int
	eventStream   *websocket.Conn
	resourceName  string
	pingConfig    PingConfig
}

func NewEventRouter(name string, priority int, apiURL string, accessKey string, secretKey string,
	apiClient *client.RancherClient, eventHandlers map[string]EventHandler, resourceName string, workerCount int,
	pingConfig PingConfig) (*EventRouter, error) {

	if apiClient == nil {
		var err error
		apiClient, err = client.NewRancherClient(&client.ClientOpts{
			Timeout:   time.Second * 30,
			Url:       apiURL,
			AccessKey: accessKey,
			SecretKey: secretKey,
		})
		if err != nil {
			return nil, err
		}
	}

	// TODO Get subscribe collection URL from API instead of hard coding
	subscribeURL := strings.Replace(apiURL+"/subscribe", "http", "ws", -1)

	return &EventRouter{
		name:          name,
		priority:      priority,
		apiURL:        apiURL,
		accessKey:     accessKey,
		secretKey:     secretKey,
		apiClient:     apiClient,
		subscribeURL:  subscribeURL,
		eventHandlers: eventHandlers,
		workerCount:   workerCount,
		resourceName:  resourceName,
		pingConfig:    pingConfig,
	}, nil
}

// The difference between Start and StartWithoutCreate is a matter of making this event router
// more generally usable. The Start implementation creates
// the necessary ExternalHandler upon start up. This router has been refactor to
// be used in situations where creating an externalHandler is not desired.
// This allows the router to be used for Agent connections and for ExternalHandlers
// that are created outside of this router.

func (router *EventRouter) Start(ready chan<- bool) error {
	err := router.createExternalHandler()
	if err != nil {
		return err
	}
	eventSuffix := ";handler=" + router.name
	return router.run(ready, eventSuffix)
}

func (router *EventRouter) StartWithoutCreate(ready chan<- bool) error {
	return router.run(ready, "")
}

func (router *EventRouter) run(ready chan<- bool, eventSuffix string) (err error) {
	workers := make(chan *Worker, router.workerCount)
	for i := 0; i < router.workerCount; i++ {
		w := newWorker()
		workers <- w
	}

	log.WithFields(log.Fields{
		"workerCount": router.workerCount,
	}).Info("Initializing event router")

	handlers := map[string]EventHandler{}

	if pingHandler, ok := router.eventHandlers["ping"]; ok {
		// Ping doesnt need registered in the POST and ping events don't have the handler suffix.
		//If we start handling other non-suffix events, we might consider improving this.
		handlers["ping"] = pingHandler
	}

	subscribeParams := url.Values{}
	for event, handler := range router.eventHandlers {
		fullEventKey := event + eventSuffix
		subscribeParams.Add("eventNames", fullEventKey)
		handlers[fullEventKey] = handler
	}

	eventStream, err := router.subscribeToEvents(router.subscribeURL, router.accessKey, router.secretKey, subscribeParams)
	if err != nil {
		return err
	}
	log.Info("Connection established")
	router.eventStream = eventStream
	defer router.Stop()

	if ready != nil {
		ready <- true
	}

	ph := newPongHandler(router)
	defer ph.stop()
	router.eventStream.SetPongHandler(ph.handle)
	go router.sendWebsocketPings()

	for {

		_, message, err := router.eventStream.ReadMessage()
		if err != nil {
			// Error here means the connection is closed. It's normal, so just return.
			return nil
		}

		message = bytes.TrimSpace(message)
		if len(message) == 0 {
			continue
		}

		select {
		case worker := <-workers:
			go worker.DoWork(message, handlers, router.apiClient, workers)
		default:
			log.WithFields(log.Fields{
				"workerCount": router.workerCount,
			}).Info("No workers available dropping event.")
		}
	}
}

func (router *EventRouter) Run() {
	log.Info("Initializing event router")

	subscribeParams := url.Values{}
	for event := range router.eventHandlers {
		subscribeParams.Add("eventNames", event)
	}

	for errcnt := 0; ; errcnt++ {
		if err := router.connectAndHandleEvents(subscribeParams); err != nil {
			log.Warnf("Error working with websocket: %s", err)
		}
		log.Infof("Re-establishing websocket connection in %v seconds ...", errcnt)
		time.Sleep(time.Duration(errcnt) * time.Second)
	}
}

func (router *EventRouter) connectAndHandleEvents(subscribeParams url.Values) error {
	eventStream, err := router.subscribeToEvents(router.subscribeURL, router.accessKey, router.secretKey, subscribeParams)
	if err != nil {
		log.Errorf("Error getting websocket connection: %s", err)
		return err
	}
	router.eventStream = eventStream
	defer router.Stop()
	log.Info("Connection established")

	ph := newPongHandler(router)
	defer ph.stop()
	router.eventStream.SetPongHandler(ph.handle)
	go router.sendWebsocketPings()

	msgChan := make(chan []byte)
	defer close(msgChan)
	go router.readMsgChan(msgChan)

	for {
		_, message, err := router.eventStream.ReadMessage()
		if err != nil {
			// Error here means the connection is closed. It's normal, so just return.
			log.Warnf("Error reading message: websocket closed: %s", err)
			return err
		}
		message = bytes.TrimSpace(message)
		if len(message) == 0 {
			continue
		}
		msgChan <- message
	}
}

func (router *EventRouter) readMsgChan(msgChan chan []byte) {
	for msg := range msgChan {
		router.processMessage(msg)
	}
}

func (router *EventRouter) processMessage(rawEvent []byte) {
	var event *Event
	if err := json.Unmarshal(rawEvent, &event); err != nil {
		log.Errorf("Error unmarshalling event: %s", err)
		return
	}

	if event.Name != "ping" {
		log.WithFields(log.Fields{
			"event": string(rawEvent[:]),
		}).Debug("Processing event.")
	}

	if fn, ok := router.eventHandlers[event.Name]; ok {
		go func() {
			if err := fn(event, router.apiClient); err != nil {
				log.WithFields(log.Fields{
					"eventName":  event.Name,
					"eventId":    event.ID,
					"resourceId": event.ResourceID,
				}).Errorf("Error processing event: %s", err)
			}
		}()
	} else {
		log.WithFields(log.Fields{
			"eventName": event.Name,
		}).Warn("No event handler registered for event")
	}
}

func (router *EventRouter) Stop() {
	router.eventStream.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	router.eventStream.Close()
}

func (router *EventRouter) subscribeToEvents(subscribeURL string, accessKey string, secretKey string, data url.Values) (*websocket.Conn, error) {
	dialer := &websocket.Dialer{}
	headers := http.Header{}
	headers.Add("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(accessKey+":"+secretKey)))
	subscribeURL = subscribeURL + "?" + data.Encode()
	ws, resp, err := dialer.Dial(subscribeURL, headers)

	if err != nil {
		log.WithFields(log.Fields{
			"subscribeUrl":    subscribeURL,
		}).Errorf("Error subscribing to events: %s", err)
		if resp != nil {
			log.WithFields(log.Fields{
				"status":          resp.Status,
				"statusCode":      resp.StatusCode,
				"responseHeaders": resp.Header,
			}).Error("Got error response")
			if resp.Body != nil {
				defer resp.Body.Close()
				body, _ := ioutil.ReadAll(resp.Body)
				log.Errorf("Error response: %s", body)
			}
		}
		return nil, err
	}
	return ws, nil
}
