package main

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"net"
	"time"
)

type Room struct {
	Join   chan *Client
	Closed chan *Client
	// reader をスムースに動かしても room や sender が間に合わないと仕方がない.
	// unbuffered chan を使って入力を制限する.
	Recv chan string // received message from clients.
	// slice の resize コストが接続数に比例して増えるのを防ぐため map を使う.
	clients map[*Client]bool
}

func newRoom() *Room {
	r := &Room{
		Join:    make(chan *Client),
		Closed:  make(chan *Client),
		Recv:    make(chan string),
		clients: make(map[*Client]bool),
	}
	go func() {
		for {
			select {
			case c := <-r.Join:
				log.Printf("Room: %v is joined", c)
				r.clients[c] = true
			case c := <-r.Closed: // c は停止済み.
				if r.clients[c] {
					delete(r.clients, c)
					log.Printf("Room: %v has been closed", c)
				}
			case msg := <-r.Recv:
				for c := range r.clients {
					if err := c.Send(msg); err != nil {
						// 受信が詰まったクライアントは止める.
						log.Println(err)
						c.Stop()
						delete(r.clients, c)
					}
				}
				log.Printf("Room: Received %#v", msg)
			}
		}
	}()
	return r
}

type Client struct {
	id   int
	recv chan string
	send chan string
	stop chan bool
	conn *net.TCPConn
}

func (c *Client) String() string {
	return fmt.Sprintf("Client(%v)", c.id)
}

var lastClientId = 0

// lastClientId のせいでスレッドセーフじゃないよごめん
func newClient(r *Room, conn *net.TCPConn) *Client {
	lastClientId += 1
	cl := &Client{
		id:   lastClientId,
		recv: r.Recv,
		// クライアントは一時的に接続が不安定になるかもしれないのでバッファ多め
		send: make(chan string, 50),
		stop: make(chan bool),
		conn: conn,
	}
	go cl.sender()
	go cl.receiver()
	log.Print("%v created", cl)
	return cl
}

// Send msg to the client.
func (c *Client) Send(msg string) error {
	select {
	case c.send <- msg:
		return nil
	case <-time.After(time.Millisecond * 10):
		// room goroutine が速すぎて send が間に合ってないだけかもしれないので、少しは待つ
		return errors.New("Can't send to client")
	}
}

// Stop closes client.  It can be called multiple times.
func (c *Client) Stop() {
	// defer recover() ではダメ
	defer func() {
		r := recover()
		if r != nil {
			log.Println(r)
		}
	}()
	close(c.stop)
}

func (c *Client) sender() {
	defer c.conn.Close()
	for {
		select {
		case <-c.stop:
			return
		case msg := <-c.send:
			//log.Printf("sender: %#v", msg)
			_, err := c.conn.Write([]byte(msg))
			// net.Error.Temporary() をハンドルする必要は多分ない.
			// 書き込みできたバイト数が len(msg) より小さい場合は必ず err != nil なので、
			// Write() の第一戻り値は無視してエラーハンドリングだけを行う
			if err != nil {
				log.Println(err)
				return
			}
		}
	}
}

func (c *Client) receiver() {
	defer c.Stop() // receiver が止まるときはかならず全体を止める.
	reader := bufio.NewReader(c.conn)
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
	room := newRoom()

	addr, err := net.ResolveTCPAddr("tcp", "0.0.0.0:5056")
	if err != nil {
		log.Fatal(err)
	}
	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}

	for {
		conn, err := l.AcceptTCP()
		if err != nil {
			log.Println(err)
			conn.Close()
			continue
		}
		room.Join <- newClient(room, conn)
	}
}
