package main

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

type Room struct {
	Join   chan *Client
	Closed chan *Client
	// reader をスムースに動かしても room や sender が間に合わないと仕方がない.
	// unbuffered chan を使って入力を制限する.
	Recv  chan string // received message from clients.
	Purge chan bool
	Stop  chan bool
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

const CHAT_LOG_LENGTH = 100

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
	id     int
	recv   chan string
	closed chan *Client
	send   chan string
	stop   chan bool
	conn   *net.TCPConn
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
		id:     lastClientId,
		recv:   r.Recv,
		closed: r.Closed,
		// クライアントは一時的に接続が不安定になるかもしれないのでバッファ多め
		// CHAT_LOG_LENGTH 以上なら、ログイン後の sendLog がすぐ終わることを保障できる.
		send: make(chan string, CHAT_LOG_LENGTH+1),
		stop: make(chan bool),
		conn: conn,
	}
	clientWait.Add(1) // receiver の終了を待つ必要は特にないので sender 分だけ Add
	go cl.sender()
	go cl.receiver()
	log.Printf("%v is created", cl)
	return cl
}

// Send msg to the client.
func (c *Client) Send(msg string) error {
	// 特定のクライアントが遅いときに、全体を遅くしたくないので、 select を使って
	// すぐに送信できない場合は諦める。
	// ただし、 room goroutine が sender goroutine より速く回ってるためにチャンネルの
	// バッファがいっぱいになってるだけの可能性もあるので、一定時間は待つ.
	// バッファに空きがあるときに時に time.After を生成するのは無駄なので、 select を2段にする.
	select {
	case c.send <- msg:
		return nil
	default:
		select {
		case c.send <- msg:
			return nil
		case <-time.After(time.Millisecond * 10):
			return errors.New("Can't send to client")
		}
	}
}

// Stop closes client.  It can be called multiple times.
func (c *Client) Stop() {
	// 1つのチャンネルで複数の goroutine を止められるように、メッセージ送信じゃなくて
	// close を使う。
	// (このサンプルプログラムでは stop 受信してるのは1箇所だけなのでメッセージ送信でも可)
	// ちなみに defer recover() ではダメ
	defer func() {
		if r := recover(); r != nil {
			log.Println(r)
		}
	}()
	close(c.stop)
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
	for {
		select {
		case <-c.stop:
			//log.Println("sender: Received stop")
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
	// 1行を512byteに制限する.
	reader := bufio.NewReaderSize(c.conn, 512)
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
				if nerr, ok := err.(net.Error); ok && nerr.Temporary() {
					continue
				}
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
