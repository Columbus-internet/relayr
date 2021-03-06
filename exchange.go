package relayr

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ClientScriptFunc is a callback for altering the client side
// generated Javascript. This can be used to minify/alter the
// generated client-side RelayR library before it gets to the browser.
var ClientScriptFunc func([]byte) []byte

// A cache for the client script to avoid regenerating it every
// single page load
var clientScript []byte
var cacheEnabled = true

// DisableScriptCache forces the RelayR client-side script to
// be regenerated on each request, rather than serving it from
// an internal cache.
func DisableScriptCache() {
	cacheEnabled = false
}

var upgrader = &websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type longPollServerCall struct {
	Server       bool          `json:"S"`
	Relay        string        `json:"R"`
	Method       string        `json:"M"`
	Arguments    []interface{} `json:"A"`
	ConnectionID string        `json:"C"`
}

// Exchange represents a hub where clients exchange information
// via Relays. Relays registered with the Exchange expose methods
// that can be invoked by clients.
type Exchange struct {
	relays               []Relay
	groups               map[string][]*client
	transports           map[string]Transport
	mainURL              string
	mainURLWithoutScheme string
	mapLock              sync.Mutex
	verbosity            int
}

type negotiation struct {
	T string // the transport that the client is comfortable using (e.g, websockets)
}

type negotiationResponse struct {
	ConnectionID string
}

// NewExchange initializes and returns a new Exchange
func NewExchange(mainURL string, verbosity int) *Exchange {
	e := &Exchange{}
	e.groups = make(map[string][]*client)
	e.transports = map[string]Transport{
		"websocket": newWebSocketTransport(e),
		"longpoll":  newLongPollTransport(e),
	}
	e.mainURL = mainURL
	e.mainURLWithoutScheme = strings.Replace(e.mainURL, "https://", "", -1)
	e.mainURLWithoutScheme = strings.Replace(e.mainURLWithoutScheme, "http://", "", -1)
	e.verbosity = verbosity

	return e
}

func (e *Exchange) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	op := extractOperationFromURL(r)

	switch op {
	case opWebSocket:
		e.upgradeWebSocket(w, r)
	case opNegotiate:
		e.negotiateConnection(w, r)
	case opLongPoll:
		e.awaitLongPoll(w, r)
	case opCallServer:
		e.callServer(w, r)
	default:
		u := r.URL
		lastIndex := strings.LastIndex(u.Path, "/"+op)
		route := e.mainURL + u.Path[:lastIndex]
		baseURL := e.mainURLWithoutScheme + u.Path[:lastIndex]
		e.writeClientScript(w, baseURL, route)
	}
}

func extractOperationFromURL(r *http.Request) string {
	lastSlash := strings.LastIndex(r.URL.Path, "/")
	return r.URL.Path[lastSlash+1:]
}

func (e *Exchange) upgradeWebSocket(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err.Error())
		return
	}

	c := &connection{
		e:   e,
		out: make(chan []byte, 10*1024),
		ws:  ws,
		c:   e.transports["websocket"].(*webSocketTransport),
		id:  r.URL.Query()["connectionId"][0],
	}

	c.c.connected <- c
	defer func() { c.c.disconnected <- c }()

	go c.write()

	keepAlive(c, 40*time.Second)

	c.read()
}

func keepAlive(c *connection, timeout time.Duration) {
	lastResponse := time.Now()
	c.ws.SetPongHandler(func(msg string) error {
		lastResponse = time.Now()
		return nil
	})

	go func() {
		for {
			err := c.ws.WriteMessage(websocket.PingMessage, []byte("keepalive"))
			if err != nil {
				return
			}
			time.Sleep(timeout / 2)
			if time.Now().Sub(lastResponse) > timeout {
				c.ws.Close()
				return
			}
		}
	}()
}

func (e *Exchange) negotiateConnection(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w)
	decoder := json.NewDecoder(r.Body)

	var neg negotiation

	decoder.Decode(&neg)

	encoder := json.NewEncoder(w)

	e.mapLock.Lock()
	encoder.Encode(negotiationResponse{ConnectionID: e.addClient(neg.T)})
	e.mapLock.Unlock()
}

func (e *Exchange) awaitLongPoll(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w)
	cid := e.extractConnectionIDFromURL(r)
	longPoll := e.transports["longpoll"].(*longPollTransport)
	longPoll.wait(w, cid)
}

func (e *Exchange) callServer(w http.ResponseWriter, r *http.Request) {
	var msg longPollServerCall
	decoder := json.NewDecoder(r.Body)
	decoder.Decode(&msg)
	cid := e.extractConnectionIDFromURL(r)
	relay := e.getRelayByName(msg.Relay, cid)
	go e.callRelayMethod(relay, msg.Method, msg.Arguments...)
}

func (e *Exchange) extractConnectionIDFromURL(r *http.Request) string {
	return r.URL.Query()["connectionId"][0]
}

func (e *Exchange) addClient(t string) string {
	cID := generateConnectionID()
	client := &client{ConnectionID: cID, exchange: e, transport: e.transports[t]}
	// e.mapLock.Lock()
	e.groups["Global"] = append(e.groups["Global"], client)
	// e.mapLock.Unlock()
	return cID
}

func (e *Exchange) writeClientScript(w http.ResponseWriter, baseURL, route string) {
	if len(clientScript) > 0 && cacheEnabled {
		io.Copy(w, bytes.NewBuffer(clientScript))
	} else {
		resultBuff := bytes.Buffer{}
		buff := bytes.Buffer{}

		buff.WriteString(fmt.Sprintf(connectionClassScript, baseURL, route))

		buff.WriteString(relayClassBegin)

		for _, relay := range e.relays {
			buff.WriteString(fmt.Sprintf(relayBegin, relay.Name))

			for _, method := range relay.methods {
				buff.WriteString(fmt.Sprintf(relayMethod, lowerFirst(method), relay.Name, method))

			}
			buff.WriteString(relayEnd)
		}

		buff.WriteString(relayClassEnd)

		if ClientScriptFunc != nil {
			resultBuff.Write(ClientScriptFunc(buff.Bytes()))
		} else {
			resultBuff.Write(buff.Bytes())
		}

		if cacheEnabled {
			clientScript = resultBuff.Bytes()
		}

		io.Copy(w, &resultBuff)
	}
}

// RegisterRelay registers a struct as a Relay with the Exchange. This allows clients
// to invoke server methods on a Relay and allows the Exchange to invoke
// methods on a Relay on the server side.
func (e *Exchange) RegisterRelay(x interface{}) {
	t := reflect.TypeOf(x)

	methods := e.getMethodsForType(t)

	e.relays = append(e.relays, Relay{Name: t.Name(), UnderlyingStruct: x, t: t, methods: methods, exchange: e})
}

func (e *Exchange) getMethodsForType(t reflect.Type) []string {
	r := []string{}
	for i := 0; i < t.NumMethod(); i++ {
		r = append(r, t.Method(i).Name)
	}

	return r
}

func (e *Exchange) getRelayByName(name string, cID string) *Relay {
	// Create an instance of Relay
	for _, r := range e.relays {
		if r.Name == name {
			relay := &Relay{
				Name:             name,
				ConnectionID:     cID,
				t:                r.t,
				exchange:         e,
				UnderlyingStruct: r.UnderlyingStruct,
			}

			relay.Clients = &ClientOperations{
				e:     e,
				relay: relay,
			}

			return relay
		}
	}

	return nil
}

func (e *Exchange) callRelayMethod(relay *Relay, fn string, args ...interface{}) error {
	newInstance := reflect.New(relay.t)
	method := newInstance.MethodByName(fn)
	empty := reflect.Value{}
	if method == empty {
		return fmt.Errorf("Method '%v' does not exist on relay '%v'", fn, relay.Name)
	}
	method.Call(buildArgValues(relay, args...))

	return nil
}

func buildArgValues(relay *Relay, args ...interface{}) []reflect.Value {
	r := []reflect.Value{reflect.ValueOf(relay)}
	for _, a := range args {
		r = append(r, reflect.ValueOf(a))
	}

	return r
}

// Relay generates an instance of a Relay, allowing calls to be made to
// it on the server side. It is generated a random ConnectionID for the duration
// of the call and it does not represent an actual client.
func (e *Exchange) Relay(x interface{}) *Relay {
	return e.getRelayByName(reflect.TypeOf(x).Name(), generateConnectionID())
}

func (e *Exchange) callClientMethod(r *Relay, fn string, args ...interface{}) {
	if r.ConnectionID == "" {
		e.callGroupMethod(r, "Global", fn, args...)
		return
	}

	c := e.getClientByConnectionID(r.ConnectionID)
	if c != nil {
		c.transport.CallClientFunction(r, fn, args...)
	}
}

func (e *Exchange) callGroupMethod(relay *Relay, group, fn string, args ...interface{}) {
	// e.mapLock.Lock()
	// defer e.mapLock.Unlock()
	if _, ok := e.groups[group]; ok {
		if e.verbosity > 0 {
			log.Println("group found")
			log.Printf("list of clients for group when calling %s:\n", group)
		}
		for _, c := range e.groups[group] {
			if c == nil {
				if e.verbosity > 0 {
					log.Printf("c.ConnectionID will fail since c is nil, group key %s", group)
				}
				continue
			}
			if e.verbosity > 0 {
				log.Printf("ConnectionID: %s\n", c.ConnectionID)
			}
			r := e.getRelayByName(relay.Name, c.ConnectionID)
			if e.verbosity > 0 {
				log.Printf("sending to %s", c.ConnectionID)
			}
			c.transport.CallClientFunction(r, fn, args...)
		}
	} else {
		if e.verbosity > 0 {
			log.Printf("group '%s' not found. All groups: %v", group, e.groups)
		}
	}
}

func (e *Exchange) callGroupMethodExcept(relay *Relay, group, fn string, args ...interface{}) {
	// e.mapLock.Lock()
	// defer e.mapLock.Unlock()
	for _, c := range e.groups[group] {
		if c.ConnectionID == relay.ConnectionID {
			continue
		}
		r := e.getRelayByName(relay.Name, c.ConnectionID)
		c.transport.CallClientFunction(r, fn, args...)
	}
}

func (e *Exchange) getClientByConnectionID(cID string) *client {
	// e.mapLock.Lock()
	// defer e.mapLock.Unlock()
	for _, c := range e.groups["Global"] {
		if c.ConnectionID == cID {
			return c
		}
	}
	return nil
}

func (e *Exchange) removeFromAllGroups(id string) {
	if e.verbosity > 0 {
		log.Printf("removing client %s from all groups\n", id)
	}
	for group := range e.groups {
		e.removeFromGroupByID(group, id)
	}
}

func (e *Exchange) removeFromGroupByID(g, id string) {
	if e.verbosity > 0 {
		log.Printf("removing client %s from '%s'\n", id, g)
	}
	// e.mapLock.Lock()
	// defer e.mapLock.Unlock()

	if i := e.getClientIndexInGroup(g, id); i > -1 {
		group := e.groups[g]
		group[i] = nil
		e.groups[g] = append(group[:i], group[i+1:]...)

		// clean up the group if it is empty
		if len(e.groups[g]) == 0 {
			delete(e.groups, g)
		}
		if e.verbosity > 0 {
			log.Println("client removed")
		}
	} else {
		if e.verbosity > 0 {
			log.Printf("client %s not in the group '%s'", id, g)
		}
	}
}

func (e *Exchange) getClientIndexInGroup(g, id string) int {
	// e.mapLock.Lock()
	// defer e.mapLock.Unlock()
	for i, c := range e.groups[g] {
		if c != nil && c.ConnectionID == id {
			return i
		}
	}

	return -1
}

func (e *Exchange) addToGroup(group, connectionID string) {
	// only add them if they aren't currently in the group
	// e.mapLock.Lock()
	// defer e.mapLock.Unlock()
	if e.getClientIndexInGroup(group, connectionID) == -1 {
		e.groups[group] = append(e.groups[group], e.getClientByConnectionID(connectionID))
		if e.verbosity > 0 {
			log.Printf("list of clients for group %s:\n", group)
			for _, c := range e.groups[group] {
				log.Printf("ConnectionID: %s\n", c.ConnectionID)
			}
		}
	} else {
		if e.verbosity > 0 {
			log.Printf("client %s NOT added to '%s'\n", connectionID, group)
			log.Printf("list of clients for group %s:\n", group)
			for _, c := range e.groups[group] {
				log.Printf("ConnectionID: %s\n", c.ConnectionID)
			}
		}
	}
}
