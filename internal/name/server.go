package name

import (
	"net"
	"net/rpc"
	"strconv"
)

const (
	rpcName         = "NameServer"
	getNameMethod   = rpcName + ".GetName"
	blacklistMethod = rpcName + ".BlacklistName"

	maxTryCount = 16
)

type Type byte

const (
	Name Type = iota
	File
	Field
	Package
)

type Info struct {
	Type            Type
	ScopeIdentifier string
	Name            string
}

func (i *Info) Key() string {
	return strconv.Itoa(int(i.Type)) + "|" + i.ScopeIdentifier + "|" + i.Name
}

type biMap struct {
	forward  map[string]string
	backward map[string]string
}

func (b *biMap) Set(k, v string) {
	b.forward[k] = v
	b.backward[v] = k
}

func (b *biMap) GetByKey(k string) (val string, ok bool) {
	val, ok = b.forward[k]
	return
}

func (b *biMap) GetByValue(v string) (key string, ok bool) {
	key, ok = b.backward[v]
	return
}

func newBimap() *biMap {
	return &biMap{
		forward:  make(map[string]string),
		backward: make(map[string]string),
	}
}

type GetNameFunc func(info *Info, try int) string

type Generator interface {
	GetName(info *Info) string
	BlacklistName(name string)
}

type server struct {
	gen GetNameFunc

	nameMap       *biMap
	nameBlacklist map[string]bool
}

func (s *server) GetName(args *Info, newNameRes *string) error {
	key := args.Key()
	if newName, ok := s.nameMap.GetByKey(key); ok {
		*newNameRes = newName
		return nil
	}

	var newName string
	for i := 1; i < maxTryCount+1; i++ {
		tmp := s.gen(args, i)
		if args.Type != File {
			if _, ok := s.nameBlacklist[tmp]; ok {
				continue
			}
		}
		if _, ok := s.nameMap.GetByValue(tmp); ok {
			continue
		}
		newName = tmp
		break
	}

	if newName == "" {
		panic("new name not generated")
	}
	s.nameMap.Set(key, newName)
	*newNameRes = newName
	return nil
}

func (s *server) BlacklistName(name *string, res *bool) error {
	s.nameBlacklist[*name] = true
	*res = true
	return nil
}

func StartNameServer(g GetNameFunc) (string, error) {
	nsServer := rpc.NewServer()
	if err := nsServer.RegisterName(rpcName, &server{
		gen:           g,
		nameMap:       newBimap(),
		nameBlacklist: make(map[string]bool),
	}); err != nil {
		return "", err
	}

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := listener.Addr().String()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				continue
			}
			nsServer.ServeConn(conn)
		}
	}()
	return addr, nil
}

type client struct {
	*rpc.Client
}

func (c *client) GetName(info *Info) string {
	var reply string
	if err := c.Call(getNameMethod, info, &reply); err != nil {
		panic(err)
	}
	return reply
}

func (c *client) BlacklistName(name string) {
	if err := c.Call(blacklistMethod, &name, nil); err != nil {
		panic(err)
	}
}

func ConnectClient(addr string) (Generator, error) {
	c, err := rpc.Dial("tcp4", addr)
	if err != nil {
		return nil, err
	}
	return &client{c}, nil
}
