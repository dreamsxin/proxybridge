package cachef

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
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
	filename string
	data     []*Bridge
	rwm      *sync.RWMutex
}

func New(filename string) (*CacheF, error) {
	if filename == "" {
		return nil, errors.New("cache filename is required")
	}
	cf := &CacheF{
		filename: filename,
		rwm:      &sync.RWMutex{},
		data:     make([]*Bridge, 0),
	}

	content, err := os.ReadFile(filename)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Error("openfile", "filename", filename, "err", err)
			return nil, err
		}
		// 首次启动：立刻把空文件建出来，目录权限之类的问题在启动时就暴露，
		// 而不是等到第一次 /bridge/add 才失败
		if err := cf.dump(); err != nil {
			return nil, err
		}
		return cf, nil
	}
	if len(content) == 0 {
		return cf, nil
	}

	if err := cf.unmarshal(content); err != nil {
		slog.Error("unmarshal", "filename", filename, "err", err)
		return nil, err
	}
	return cf, nil
}

// dump 原子地全量重写数据文件：写临时文件 → fsync → rename 覆盖 → fsync 父目录。
//
// 旧实现是对同一个 fd 做 Truncate(0) 再重写，进程在 Truncate 与 Sync 之间挂掉
// （或机器掉电）会留下空文件或半截内容，重启后桥全部丢失或只剩一部分。
// rename 在同一文件系统内是原子的，崩溃后看到的要么是旧的完整内容、要么是新的完整内容。
//
// 调用方必须持有 rwm 写锁。
func (s *CacheF) dump() error {
	var buf bytes.Buffer
	for _, b := range s.data {
		fmt.Fprintf(&buf, "%d,%s\n", b.Port, b.ProxyAddr)
	}

	dir := filepath.Dir(s.filename)
	// 临时文件必须和目标同目录：跨文件系统的 rename 不是原子的，也可能直接失败
	tmp, err := os.CreateTemp(dir, filepath.Base(s.filename)+".tmp-*")
	if err != nil {
		slog.Error("dump create temp", "dir", dir, "err", err)
		return err
	}
	tmpName := tmp.Name()
	// 失败路径上不留垃圾临时文件；rename 成功后置空跳过清理
	defer func() {
		if tmpName != "" {
			os.Remove(tmpName)
		}
	}()

	// CreateTemp 建出来就是 0600，转发映射表不应对其他用户可读写
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		slog.Error("dump write", "count", len(s.data), "err", err)
		return err
	}
	// 先 fsync 再 rename：否则崩溃后可能出现「目录项已指向新文件、内容却还没落盘」
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		slog.Error("dump sync", "count", len(s.data), "err", err)
		return err
	}
	if err := tmp.Close(); err != nil {
		slog.Error("dump close temp", "err", err)
		return err
	}
	if err := os.Rename(tmpName, s.filename); err != nil {
		slog.Error("dump rename", "filename", s.filename, "err", err)
		return err
	}
	tmpName = ""
	syncDir(dir)
	slog.Debug("dump", "count", len(s.data))
	return nil
}

// syncDir 让 rename 这个目录项变更本身落盘。Windows 不支持打开目录做 fsync，跳过。
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	d, err := os.Open(dir)
	if err != nil {
		slog.Debug("dump open dir", "dir", dir, "err", err)
		return
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		slog.Debug("dump sync dir", "dir", dir, "err", err)
	}
}

func (s *CacheF) unmarshal(content []byte) error {
	s.rwm.Lock()
	defer s.rwm.Unlock()

	reader := bufio.NewReader(bytes.NewReader(content))
	lineNo := 0
	for {
		line, readErr := reader.ReadString('\n')
		// ReadString 在文件末尾会同时返回最后一段数据和 io.EOF，
		// 必须先处理 line 再判断 err，否则末行无换行符时该条记录会被丢弃。
		if line != "" {
			lineNo++
			if err := s.parseLineLocked(strings.TrimRight(line, "\r\n"), lineNo); err != nil {
				return err
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				slog.Error("unmarshal read", "line", lineNo, "err", readErr)
				return readErr
			}
			break
		}
	}
	slog.Info("cache loaded", "count", len(s.data))
	return nil
}

// parseLineLocked 解析一行 "port,proxyAddr" 或 "beginPort:endPort,proxyAddr"。
// 调用方必须持有 rwm 写锁。
func (s *CacheF) parseLineLocked(line string, lineNo int) error {
	if line == "" || strings.HasPrefix(line, "#") {
		return nil
	}
	record := strings.Split(line, ",")
	if len(record) != 2 {
		slog.Warn("cache skip malformed line", "line", lineNo, "content", line)
		return nil
	}
	portField := strings.TrimSpace(record[0])
	proxyAddr := strings.TrimSpace(record[1])
	if proxyAddr == "" {
		slog.Warn("cache skip empty proxyAddr", "line", lineNo, "content", line)
		return nil
	}

	if strings.Contains(portField, ":") {
		ports := strings.Split(portField, ":")
		if len(ports) != 2 {
			slog.Warn("cache skip malformed port range", "line", lineNo, "content", line)
			return nil
		}
		bPort, err := parsePort(ports[0])
		if err != nil {
			slog.Error("cache parse port range", "line", lineNo, "content", line, "err", err)
			return err
		}
		ePort, err := parsePort(ports[1])
		if err != nil {
			slog.Error("cache parse port range", "line", lineNo, "content", line, "err", err)
			return err
		}
		if bPort > ePort {
			bPort, ePort = ePort, bPort
		}
		// 用 uint32 做循环变量：ePort 为 65535 时 uint16 自增会回绕成 0 而死循环
		for p := uint32(bPort); p <= uint32(ePort); p++ {
			s.upsertLocked(uint16(p), proxyAddr)
		}
		return nil
	}

	port, err := parsePort(portField)
	if err != nil {
		slog.Warn("cache skip invalid port", "line", lineNo, "content", line, "err", err)
		return nil
	}
	s.upsertLocked(port, proxyAddr)
	return nil
}

func parsePort(field string) (uint16, error) {
	p, err := strconv.ParseUint(strings.TrimSpace(field), 10, 32)
	if err != nil {
		return 0, err
	}
	if p < 1 || p > 65535 {
		return 0, fmt.Errorf("port out of range %d", p)
	}
	return uint16(p), nil
}

// upsertLocked 覆盖同端口的已有记录而不是追加，避免出现重复端口条目
// （重复条目会让 Get/Del 只命中第一条，导致监听与缓存状态分叉）。
// 调用方必须持有 rwm 写锁。
func (s *CacheF) upsertLocked(port uint16, proxyAddr string) {
	b := &Bridge{Port: port, ProxyAddr: proxyAddr}
	for idx, cur := range s.data {
		if cur.Port == port {
			s.data[idx] = b
			return
		}
	}
	s.data = append(s.data, b)
}

func (s *CacheF) Add(port uint16, proxyAddr string) error {
	s.rwm.Lock()
	defer s.rwm.Unlock()
	s.upsertLocked(port, proxyAddr)
	return s.dump()
}

// Replace 用一份新集合原子替换全部记录，用于远端同步对账。
//
// 拒绝空集合：中心侧返回空列表（bridgeId 配错、上游数据异常、接口改动）时
// 不能把本地映射表擦掉——擦掉之后本地再没有副本可以恢复，所有桥直接消失。
// 要清空请逐个走 Del。
//
// 写盘失败会回滚内存，避免留下「内存已空、磁盘还是旧数据」的分叉：
// 那种状态下紧接着的 InitBridgeHandler 会一个桥都不起，而磁盘看起来完好。
func (s *CacheF) Replace(bridges []Bridge) error {
	if len(bridges) == 0 {
		return errors.New("refusing to replace cache with an empty bridge set")
	}
	s.rwm.Lock()
	defer s.rwm.Unlock()

	prev := s.data
	s.data = make([]*Bridge, 0, len(bridges))
	for _, b := range bridges {
		s.upsertLocked(b.Port, b.ProxyAddr)
	}
	if err := s.dump(); err != nil {
		s.data = prev
		slog.Error("cache replace rolled back", "count", len(bridges), "err", err)
		return err
	}
	slog.Info("cache replaced", "count", len(s.data))
	return nil
}

func (s *CacheF) All() []*Bridge {
	s.rwm.RLock()
	defer s.rwm.RUnlock()
	dst := make([]*Bridge, len(s.data))
	copy(dst, s.data)
	return dst
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
	// 删除全部同端口条目：历史数据文件里可能已存在重复端口
	kept := s.data[:0]
	removed := 0
	for _, cur := range s.data {
		if cur.Port == port {
			removed++
			continue
		}
		kept = append(kept, cur)
	}
	if removed == 0 {
		slog.Debug("cache del miss", "port", port)
		return nil
	}
	s.data = kept
	slog.Debug("cache del", "port", port, "removed", removed)
	return s.dump()
}

func (s *CacheF) Close() {
	s.rwm.Lock()
	defer s.rwm.Unlock()
	s.data = nil
}
