package relayr

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"

	"github.com/gorilla/websocket"
)

type connection struct {
	ws  *websocket.Conn
	out chan []byte
	c   *webSocketTransport
	id  string
	e   *Exchange
}

type webSocketTransport struct {
	connections  map[string]*connection
	connected    chan *connection
	disconnected chan *connection
	e            *Exchange
}

type webSocketClientMessage struct {
	Server       bool          `json:"S"`
	Relay        string        `json:"R"`
	Method       string        `json:"M"`
	Arguments    []interface{} `json:"A"`
	ConnectionID string        `json:"C"`
}

func newWebSocketTransport(e *Exchange) *webSocketTransport {
	c := &webSocketTransport{
		connected:    make(chan *connection),
		disconnected: make(chan *connection),
		connections:  make(map[string]*connection),
		e:            e,
	}

	go c.listen()

	return c
}

func (c *webSocketTransport) listen() {
	for {
		select {
		case conn := <-c.connected:
			log.Printf("connection added id: %s", conn.id)
			c.connections[conn.id] = conn
		case conn := <-c.disconnected:
			log.Printf("removing connection id: %s", conn.id)
			if _, ok := c.connections[conn.id]; ok {
				c.e.removeFromAllGroups(conn.id)
				delete(c.connections, conn.id)
				close(conn.out)
			}
		}
	}
}

func (c *webSocketTransport) CallClientFunction(relay *Relay, fn string, args ...interface{}) {
	buff := &bytes.Buffer{}
	encoder := json.NewEncoder(buff)

	encoder.Encode(struct {
		R string
		M string
		A []interface{}
	}{
		relay.Name,
		fn,
		args,
	})

	o := c.connections[relay.ConnectionID]

	if o != nil {
		o.out <- buff.Bytes()
	}
}

func (c *connection) read() {
	//var t = time.Now()
	defer func() { c.c.disconnected <- c }()
	for {
		_, message, err := c.ws.ReadMessage()
		if err != nil {
			log.Printf("c.read of conn id %s got %s", c.id, string(message))
			log.Printf("connection id %s, c.ws.ReadMessage() error: %s", c.id, err)
			//log.Printf("connection %s live time: %f", c.id, time.Now().Sub(t).Seconds())
			break
		}
		log.Printf("c.read of conn id %s got %s", c.id, string(message))

		var m webSocketClientMessage
		err = json.Unmarshal(message, &m)
		if err != nil {
			fmt.Println("ERR:", err)
			continue
		}

		relay := c.e.getRelayByName(m.Relay, m.ConnectionID)

		if m.Server {
			err := c.e.callRelayMethod(relay, m.Method, m.Arguments...)
			if err != nil {
				fmt.Println("ERR:", err)
			}
		} else {
			c.c.CallClientFunction(relay, m.Method, m.Arguments)
		}
	}

	c.ws.Close()
}

func (c *connection) write() {
	for message := range c.out {
		err := c.ws.WriteMessage(websocket.TextMessage, message)
		if err != nil {
			break
		}
	}
	c.ws.Close()
}
