package main

import (
	"flag"
	"github.com/Rehtt/Kit/link"
	"github.com/fsnotify/fsnotify"
	"golang.org/x/net/dns/dnsmessage"
	"gopkg.in/ini.v1"
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
	cache = make(map[string][4]byte)
	mu    sync.RWMutex
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
	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: 53})
	if err != nil {
		log.Fatalln("监听失败：", err)
	}
	defer conn.Close()
	log.Println("run")
	var (
		buf   = make([]byte, 512)
		addrs = link.NewDLink()
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
			defer msgPool.Put(msg)

			data, _ := msg.Pack()
			// 返回上游查询的结果
			if msg.Header.Response {
				conn.WriteTo(data, addrs.Pull().(*net.UDPAddr))
				return
			}

			for _, question := range msg.Questions {
				switch question.Type {
				case dnsmessage.TypeA:
					mu.RLock()
					ip, ok := cache[question.Name.String()]
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
						msg.Response = true
						data, _ := msg.Pack()
						conn.WriteTo(data, addr)
						continue
					}
				}
				// 查询上游服务器
				conn.WriteTo(data, &net.UDPAddr{IP: net.IP{8, 8, 8, 8}, Port: 53})
				addrs.Push(addr)
			}

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
	cache = make(map[string][4]byte, len(data.Section("hosts").Keys()))
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
		cache[s.Name()+"."] = b
	}
}
