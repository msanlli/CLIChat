package server

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"nhooyr.io/websocket"
)

type subscriber struct {
	msgs chan []byte
}

type PendingConnection struct {
	Response http.ResponseWriter
	Request  *http.Request
}

type ChatServer struct {
	subscriberMessageBuffer int
	publishLimiter          *rate.Limiter
	logf                    func(f string, v ...interface{})
	subscribers             chan *subscriber
	shutdownChan            chan struct{}
	connCh                  chan *websocket.Conn
	readyCh                 chan bool
	pendingConns            []*websocket.Conn
	clients                 map[string]*websocket.Conn
	chatSessions            map[string]string
}

func NewChatServer() *ChatServer {
	cs := &ChatServer{
		subscriberMessageBuffer: 16,
		logf:                    log.Printf,
		subscribers:             make(chan *subscriber, 100), // Assuming a maximum of 100 subscribers for simplicity.
		publishLimiter:          rate.NewLimiter(rate.Every(time.Millisecond*100), 8),
		shutdownChan:            make(chan struct{}),
		connCh:                  make(chan *websocket.Conn),
		readyCh:                 make(chan bool, 2),
		pendingConns:            make([]*websocket.Conn, 0),
		clients:                 make(map[string]*websocket.Conn),
		chatSessions:            make(map[string]string),
	}
	return cs
}

func (cs *ChatServer) addSubscriber(s *subscriber) {
	cs.subscribers <- s
}

func (s *subscriber) Close() {
	close(s.msgs)
}

func (cs *ChatServer) wsHandler(w http.ResponseWriter, r *http.Request) {
	clientID := r.URL.Query().Get("client_id")
	wsConn, err := websocket.Accept(w, r, nil)
	if err != nil {
		http.Error(w, "WebSocket handshake failed", http.StatusInternalServerError)
		return
	}

	cs.clients[clientID] = wsConn

	for {
		_, msg, err := wsConn.Read(context.Background())
		if err != nil {
			break
		}

		targetClient, ok := cs.chatSessions[clientID]
		if !ok {
			continue
		}

		targetWs := cs.clients[targetClient]
		if targetWs != nil {
			targetWs.Write(context.Background(), websocket.MessageText, msg)
		}
	}

	delete(cs.clients, clientID)
	delete(cs.chatSessions, clientID)
}

func (cs *ChatServer) connectHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Expected POST protocol", http.StatusMethodNotAllowed)
		return
	}

	clientID := r.URL.Query().Get("client_id")
	targetClient := r.URL.Query().Get("target_client")
	if clientID == "" || targetClient == "" {
		http.Error(w, "Missing client_id or target_client", http.StatusBadRequest)
		return
	}

	_, clientExists := cs.clients[clientID]
	targetWs, targetExists := cs.clients[targetClient]

	if clientExists || (targetExists && targetWs != nil) {
		http.Error(w, "Client already connected or target busy", http.StatusConflict)
		return
	}

	if !targetExists {
		cs.clients[clientID] = nil
		cs.chatSessions[targetClient] = clientID
		cs.chatSessions[clientID] = targetClient
		w.WriteHeader(http.StatusOK)
		return
	}

	w.Write([]byte("Ready for WebSocket handshake at =" + clientID))
}

func (cs *ChatServer) ListenAndServe(addr string) error {
	http.HandleFunc("/connect", cs.connectHandler)
	http.HandleFunc("/subscribe", cs.subscribeHandler)
	http.HandleFunc("/publish", cs.publishHandler)
	http.HandleFunc("/ws", cs.wsHandler)
	return http.ListenAndServe(addr, nil)
}

func (cs *ChatServer) subscribeHandler(w http.ResponseWriter, r *http.Request) {
	err := cs.subscribe(r.Context(), w, r)
	if errors.Is(err, context.Canceled) {
		return
	}
	if websocket.CloseStatus(err) == websocket.StatusNormalClosure ||
		websocket.CloseStatus(err) == websocket.StatusGoingAway {
		return
	}
	if err != nil {
		cs.logf("%v", err)
		return
	}
}

func (cs *ChatServer) publishHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "None GET protocol detected", http.StatusMethodNotAllowed)
		return
	}
	body := http.MaxBytesReader(w, r.Body, 8192)
	msg, err := io.ReadAll(body)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
		return
	}

	s := &subscriber{}
	cs.publish(s, msg)

	w.WriteHeader(http.StatusAccepted)
}

func (cs *ChatServer) subscribe(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	var mu sync.Mutex
	var c *websocket.Conn
	var closed bool
	s := &subscriber{
		msgs: make(chan []byte, cs.subscriberMessageBuffer),
	}
	defer s.Close()
	cs.addSubscriber(s)

	c2, err := websocket.Accept(w, r, nil)
	if err != nil {
		return err
	}
	mu.Lock()
	if closed {
		mu.Unlock()
		return net.ErrClosed
	}
	c = c2
	mu.Unlock()
	defer c.CloseNow()

	ctx = c.CloseRead(ctx)

	for {
		select {
		case msg := <-s.msgs:
			err := writeTimeout(ctx, time.Second*5, c, msg)
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (cs *ChatServer) publish(sender *subscriber, msg []byte) {
	cs.publishLimiter.Wait(context.Background())
	for s := range cs.subscribers {
		if s != sender {
			select {
			case s.msgs <- msg:
			default:
			}
		}
	}
}

func (cs *ChatServer) Shutdown(ctx context.Context) {
	close(cs.shutdownChan)
}

func writeTimeout(ctx context.Context, timeout time.Duration, c *websocket.Conn, msg []byte) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return c.Write(ctx, websocket.MessageText, msg)
}
