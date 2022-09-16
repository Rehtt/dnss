package main

import (
	"bytes"
	"flag"
	"github.com/fsnotify/fsnotify"
	"golang.org/x/net/dns/dnsmessage"
	"gopkg.in/ini.v1"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

var (
	confPath = flag.String("conf", "hosts.ini", "配置文件")
	msgPool  = sync.Pool{New: func() any {
		return new(dnsmessage.Message)
	}}
	cacheHost = make(map[string][4]byte)
	mu        sync.RWMutex
)

func init() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatalln("监听文件失败：", err)
	}
	err = watcher.Add(*confPath)
	if err != nil {
		log.Fatalln("监听文件失败：", err)
	}
	go func() {
		for {
			select {
			case _, ok := <-watcher.Events:
				if !ok {
					return
				}
				parsefile()
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Println("error:", err)
			}
		}
	}()
	parsefile()
}

func main() {
	flag.Parse()
	go udp()
	tcp()
}
func tcp() {
	l, err := net.ListenTCP("tcp", &net.TCPAddr{Port: 53})
	if err != nil {
		log.Fatalln("tcp监听失败：", err)
	}
	defer l.Close()
	log.Println("tcp run")
	for {
		conn, err := l.Accept()
		if err != nil {
			continue
		}
		data, err := readAll(conn)
		if err != nil {
			continue
		}
		// 暂时不对tcp解包，直接转发
		// 查询上游服务器
		rconn, err := net.DialTCP("tcp", nil, &net.TCPAddr{IP: net.IP{8, 8, 8, 8}, Port: 53})
		if err != nil {
			log.Println("tcp r1 err:", err)
			return
		}
		rconn.Write(data)
		data, err = readAll(rconn)
		if err != nil {
			log.Println("tcp r2 err:", err)
			return
		}
		rconn.Close()
		conn.Write(data)
		conn.Close()
	}
}
func readAll(reader io.Reader) ([]byte, error) {
	var out bytes.Buffer
	buf := make([]byte, 512)
	for {
		n, err := reader.Read(buf)
		if err != nil {
			return nil, err
		}
		out.Write(buf[:n])
		if n < 512 {
			return out.Bytes(), nil
		}
	}
}

type cache struct {
	msg      *dnsmessage.Message
	addr     *net.UDPAddr
	question *int32
}

func udp() {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: 53})
	if err != nil {
		log.Fatalln("udp监听失败：", err)
	}
	defer conn.Close()
	log.Println("udp run")
	var (
		buf      = make([]byte, 512)
		msgCache = make(map[uint16]*cache)
		lock     sync.Mutex
	)

	for {
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			log.Println("err1:", err)
			continue
		}
		msg := msgPool.Get().(*dnsmessage.Message)
		if err = msg.Unpack(buf[:n]); err != nil {
			log.Println("dns unpack err:", err)
			continue
		}

		go func(addr *net.UDPAddr, conn *net.UDPConn, msg *dnsmessage.Message) {
			// 上游查询的结果
			if msg.Header.Response {
				defer msgPool.Put(msg)
				lock.Lock()
				defer lock.Unlock()
				if data, ok := msgCache[msg.ID]; ok {
					data.msg.Answers = append(data.msg.Answers, msg.Answers...)
					atomic.AddInt32(data.question, -1)
					if atomic.LoadInt32(data.question) == 0 {
						dnsData, _ := data.msg.Pack()
						data.msg.Response = true
						conn.WriteTo(dnsData, data.addr)
						delete(msgCache, msg.ID)
						return
					}
				}
				return
			}

			for i, question := range msg.Questions {
				switch question.Type {
				case dnsmessage.TypeA:
					mu.RLock()
					ip, ok := cacheHost[question.Name.String()]
					mu.RUnlock()
					if ok {
						log.Println(addr.String(), "->", question.Name.String(), ip)
						msg.Answers = append(msg.Answers, dnsmessage.Resource{
							Header: dnsmessage.ResourceHeader{
								Name:  question.Name,
								Class: question.Class,
								TTL:   600,
							},
							Body: &dnsmessage.AResource{A: ip},
						})
						continue
					}
				}
				lock.Lock()
				// 查询上游服务器
				c, ok := msgCache[msg.ID]
				if !ok {
					c = &cache{
						msg:      msg,
						addr:     addr,
						question: new(int32),
					}
					msgCache[msg.ID] = c
				}
				atomic.AddInt32(c.question, 1)
				queryRemote := *msg
				queryRemote.Questions = []dnsmessage.Question{msg.Questions[i]}
				lock.Unlock()

				data, _ := queryRemote.Pack()
				conn.WriteTo(data, &net.UDPAddr{IP: net.IP{8, 8, 8, 8}, Port: 53})
			}
			lock.Lock()
			if _, ok := msgCache[msg.ID]; !ok && !msg.Response {
				msg.Response = true
				data, _ := msg.Pack()
				conn.WriteTo(data, addr)
				msgPool.Put(msg)
			}
			defer lock.Unlock()

		}(addr, conn, msg)
	}
}

func parsefile() {
	mu.Lock()
	defer mu.Unlock()
	data, err := ini.Load(*confPath)
	if err != nil {
		return
	}
	cacheHost = make(map[string][4]byte, len(data.Section("hosts").Keys()))
	for _, s := range data.Section("hosts").Keys() {
		ip := strings.Split(s.String(), ".")
		if len(ip) != 4 {
			continue
		}
		b := [4]byte{}
		for i, v := range ip {
			num, err := strconv.Atoi(v)
			if err != nil {
				continue
			}
			if num < 0 || num > 255 {
				continue
			}
			b[i] = byte(num)
		}
		cacheHost[s.Name()+"."] = b
	}
}
