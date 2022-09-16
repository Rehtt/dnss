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
	question int
}

func udp() {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: 53})
	if err != nil {
		log.Fatalln("udp监听失败：", err)
	}
	defer conn.Close()
	log.Println("udp run")
	var (
		buf = make([]byte, 512)

		msgCache sync.Map
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

		if msg.Header.Response {
			go func(conn *net.UDPConn, msg *dnsmessage.Message) {
				a, ok := msgCache.LoadAndDelete(msg.ID)
				if !ok {
					return
				}
				addr := a.(*net.UDPAddr)
				for i, v := range msg.Answers {
					switch v.Header.Type {
					case dnsmessage.TypeA:
						mu.RLock()
						ip, ok := cacheHost[v.Header.Name.String()]
						mu.RUnlock()
						if ok {
							log.Println(addr.String(), "->", v.Header.Name.String(), ip)
							msg.Answers[i].Body = &dnsmessage.AResource{A: ip}
							continue
						}
					}
				}
				data, _ := msg.Pack()
				conn.WriteTo(data, addr)
			}(conn, msg)
		} else {
			msgCache.Store(msg.ID, addr)
			conn.WriteTo(buf[:n], &net.UDPAddr{IP: net.IP{8, 8, 8, 8}, Port: 53})
		}
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
