package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

const CHAT_LINE_SIZE = 512
const CHAT_LOG_LENGTH = 100
const SEND_BUFFER_SIZE = 4 * 1024 * 1024

var BufferFull error = errors.New("Buffer full")

type Room struct {
	Join   chan *Client // new clients
	Closed chan *Client // leaved clients
	Recv   chan string  // received message from clients.
	Purge  chan bool    // kick all clients in this room.
	Stop   chan bool    // close this room.
	// slice の resize コストが接続数に比例して増えるのを防ぐため map を使う.
	clients map[*Client]bool
	log     []string
}

func newRoom() *Room {
	r := &Room{
		Join:    make(chan *Client),
		Closed:  make(chan *Client),
		Recv:    make(chan string),
		Purge:   make(chan bool),
		Stop:    make(chan bool),
		clients: make(map[*Client]bool),
	}
	go r.run()
	return r
}

func (r *Room) run() {
	defer log.Println("Room closed.")
	for {
		select {
		case c := <-r.Join:
			log.Printf("Room: %v is joined", c)
			if err := r.sendLog(c); err != nil {
				log.Println(err)
				c.Stop()
			} else {
				r.clients[c] = true
			}
		case c := <-r.Closed: // c は停止済み.
			log.Printf("Room: %v has been closed", c)
			// delete は指定されたキーが無かったら何もしない
			delete(r.clients, c)
		case msg := <-r.Recv:
			//log.Printf("Room: Received %#v", msg)
			r.appendLog(msg)
			for c := range r.clients {
				if err := c.Send(msg); err != nil {
					// 接続しただけで受信しない攻撃でバッファ食いつぶさないように、
					// 受信が詰まったクライアントは止める.
					log.Println(err)
					c.Stop()
					delete(r.clients, c)
				}
			}
		case <-r.Purge:
			log.Printf("Purge all clients")
			r.purge()
		case <-r.Stop:
			log.Println("Closing room...")
			r.purge()
			return
		}
	}
}

func (r *Room) appendLog(msg string) {
	r.log = append(r.log, msg)
	if len(r.log) > CHAT_LOG_LENGTH {
		r.log = r.log[len(r.log)-CHAT_LOG_LENGTH:]
	}
}

// sendLog sends chat log.
func (r *Room) sendLog(c *Client) error {
	for _, msg := range r.log {
		if err := c.Send(msg); err != nil {
			return err
		}
	}
	return nil
}

func (r *Room) purge() {
	for c := range r.clients {
		c.Stop()
		delete(r.clients, c)
	}
}

type Client struct {
	m       sync.Mutex
	id      int
	recv    chan string
	closed  chan *Client
	conn    *net.TCPConn
	stop    bool
	cond    sync.Cond
	sendBuf *bytes.Buffer
}

func (c *Client) String() string {
	// ログに書くオブジェクトには String() を実装しよう.
	return fmt.Sprintf("Client(%v)", c.id)
}

var lastClientId = 0
var clientWait sync.WaitGroup

func newClient(r *Room, conn *net.TCPConn) *Client {
	// lastClientId のせいでこの関数はスレッドセーフじゃないよごめん
	lastClientId++
	cl := &Client{
		m:       sync.Mutex{},
		id:      lastClientId,
		recv:    r.Recv,
		closed:  r.Closed,
		conn:    conn,
		stop:    false,
		sendBuf: &bytes.Buffer{},
	}
	cl.cond.L = &cl.m
	clientWait.Add(1) // receiver の終了を待つ必要は特にないので sender 分だけ Add
	go cl.sender()
	go cl.receiver()
	log.Printf("%v is created", cl)
	return cl
}

// Send msg to the client.
func (c *Client) Send(msg string) error {
	c.m.Lock()
	defer c.m.Unlock()

	if c.sendBuf.Len()+len(msg) > SEND_BUFFER_SIZE {
		return BufferFull
	}
	c.sendBuf.WriteString(msg)
	c.cond.Signal()
	return nil
}

// Stop closes client.  It can be called multiple times.
func (c *Client) Stop() {
	c.m.Lock()
	c.stop = true
	c.m.Unlock()
	c.cond.Signal()
}

func (c *Client) sender() {
	defer func() {
		if err := c.conn.Close(); err != nil {
			log.Println(err)
		}
		log.Printf("%v is closed", c)
		clientWait.Done()
		c.closed <- c
	}()

	buf := &bytes.Buffer{}

	c.m.Lock()
	for {
		if c.stop {
			return
		}
		if c.sendBuf.Len() == 0 {
			c.cond.Wait()
			continue
		}
		// swap & unlock で room を待たせない
		buf, c.sendBuf = c.sendBuf, buf
		c.m.Unlock()

		_, err := c.conn.Write(buf.Bytes())
		if err != nil {
			log.Println(c, err)
			return
		}
		buf.Reset()
		c.m.Lock()
	}
}

func (c *Client) receiver() {
	defer c.Stop() // receiver が止まるときはかならず全体を止める.
	// 1行を512byteに制限する.
	reader := bufio.NewReaderSize(c.conn, CHAT_LINE_SIZE)
	for {
		// Read() する goroutine を中断するのは難しい。
		// 外側で conn.Close() して、エラーで死ぬのが楽.
		// エラーは io.EOF であるとは限らない。
		msg, err := reader.ReadString(byte('\n'))
		if msg != "" {
			//log.Printf("receiver: Received %#v", msg)
			c.recv <- msg
		}
		if err != nil {
			log.Println("receiver: ", err)
			return
		}
	}
}

func main() {
	log.SetFlags(log.Lmicroseconds | log.Lshortfile)
	log.Println("PID: ", os.Getpid())
	room := newRoom()

	addr, err := net.ResolveTCPAddr("tcp", "0.0.0.0:5056")
	if err != nil {
		log.Fatal(err)
	}
	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}

	go func() {
		for {
			conn, err := l.AcceptTCP()
			if err != nil {
				log.Println(err)
				l.Close()
				return
			}
			room.Join <- newClient(room, conn)
		}
	}()

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGUSR1, syscall.SIGTERM, os.Interrupt)
	for sig := range sigc {
		switch sig {
		case syscall.SIGUSR1:
			room.Purge <- true
		case syscall.SIGTERM, os.Interrupt:
			l.Close()
			room.Stop <- true
			// 全ての client の sender を待つ
			clientWait.Wait()
			// おわり
			return
		}
	}
}
