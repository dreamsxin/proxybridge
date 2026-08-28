package cachef

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
)

type Bridge struct {
	Port      uint16 `json:"port" csv:"port"`           //端口
	ProxyAddr string `json:"proxyAddr" csv:"proxyAddr"` //代理地址
	//UpdatedAt int64  `json:"updatedAt" csv:"updatedAt"` //更新时间
}

type CacheF struct {
	file *os.File
	data []*Bridge
	rwm  *sync.RWMutex
}

func New(filename string) (*CacheF, error) {
	f, err := os.OpenFile(filename, os.O_RDWR|os.O_CREATE, os.ModePerm)
	if err != nil {
		slog.Error("openfile", "err", err)
		return nil, err
	}
	cf := &CacheF{
		file: f,
		rwm:  &sync.RWMutex{},
	}

	fi, err := f.Stat()
	if err != nil {
		fmt.Println("Stat err")
		return nil, err
	}
	if fi.Size() == 0 {
		cf.data = make([]*Bridge, 0)
		return cf, nil
	}

	if err := cf.unmarshal(); err != nil {
		fmt.Println("Size = unmarshal")
		slog.Error("unmarshal", "err", err)
		return nil, err
	}
	return cf, nil
}

func (s *CacheF) dump() error {
	s.file.Truncate(0)
	s.file.Seek(0, 0)

	records := make([]byte, 0, 1024)
	for _, b := range s.data {
		fmt.Println("port", b.Port)
		record := fmt.Sprintf("%d,%s\n", b.Port, b.ProxyAddr)
		records = append(records, []byte(record)...)
	}
	s.file.Write(records)
	return nil
}

func (s *CacheF) unmarshal() error {
	s.rwm.Lock()
	defer s.rwm.Unlock()
	reader := bufio.NewReader(s.file)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break // 到文件末尾或发生错误
		}
		line = strings.TrimRight(line, "\r\n")
		fmt.Println("line", line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		record := strings.Split(line, ",")
		if len(record) == 2 {
			if strings.Contains(record[0], ":") {
				ports := strings.Split(record[0], ":")
				if len(ports) == 2 {
					bPort, err := strconv.ParseUint(ports[0], 10, 64)
					if err != nil {
						return err
					}
					if bPort < 1 || bPort > 65535 {
						return fmt.Errorf("port err %d", bPort)
					}
					ePort, err := strconv.ParseUint(ports[1], 10, 64)
					if err != nil {
						return err
					}

					if ePort < 1 || ePort > 65535 {
						return fmt.Errorf("port err %d", bPort)
					}

					if bPort > ePort {
						t := bPort
						bPort = ePort
						ePort = t
					}
					for port := bPort; port <= ePort; port++ {
						b := &Bridge{
							Port:      uint16(port),
							ProxyAddr: record[1],
						}
						s.data = append(s.data, b)
					}
				}
			} else {
				port, err := strconv.ParseUint(record[0], 10, 64)
				if err != nil {
					continue
				}
				b := &Bridge{
					Port:      uint16(port),
					ProxyAddr: record[1],
				}
				s.data = append(s.data, b)
			}
		}
	}
	return nil

	//return gocsv.UnmarshalFile(s.file, &s.data)
}

func (s *CacheF) Add(port uint16, proxyAddr string) error {
	s.rwm.Lock()
	defer s.rwm.Unlock()
	b := &Bridge{
		Port:      port,
		ProxyAddr: proxyAddr,
	}
	flag := true
	for idx, cur := range s.data {
		if cur.Port == port {
			s.data[idx] = b
			flag = false
			break
		}
	}
	if flag {
		s.data = append(s.data, b)
	}
	return s.dump()
}

func (s *CacheF) BatchAdd(bridges []Bridge) error {
	s.rwm.Lock()
	defer s.rwm.Unlock()
	for _, bridge := range bridges {
		flag := true
		for idx, cur := range s.data {
			if cur.Port == bridge.Port {
				s.data[idx] = &bridge
				flag = false
				break
			}
		}
		if flag {
			s.data = append(s.data, &bridge)
		}
	}
	fmt.Println(s.data)
	return s.dump()
}

func (s *CacheF) All() []*Bridge {
	s.rwm.RLock()
	defer s.rwm.RUnlock()
	dst := make([]*Bridge, len(s.data))
	copy(dst, s.data)
	return dst
}

func (s *CacheF) Clear() {
	s.rwm.Lock()
	defer s.rwm.Unlock()
	s.data = make([]*Bridge, 0)
}

func (s *CacheF) Get(port uint16) *Bridge {
	s.rwm.RLock()
	defer s.rwm.RUnlock()
	for _, cur := range s.data {
		if cur.Port == port {
			return cur
		}
	}
	return nil
}

func (s *CacheF) Del(port uint16) error {
	s.rwm.Lock()
	defer s.rwm.Unlock()
	flag := false
	for idx, cur := range s.data {
		if cur.Port == port {
			s.data = append(s.data[:idx], s.data[idx+1:]...)
			flag = true
			break
		}
	}
	if flag {
		return s.dump()
	}
	return nil
}

func (s *CacheF) Close() {
	s.data = nil
	if s.file != nil {
		s.file.Close()
	}
}
