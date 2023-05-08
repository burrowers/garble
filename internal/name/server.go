package name

import (
	"net"
	"net/rpc"
)

type Server struct {
	generator Generator
}

func (s *Server) GetPackageName(args *PackageInfo, newName *string) error {
	*newName = s.generator.GetPackageName(args)
	return nil
}

func (s *Server) GetFieldName(args *FieldInfo, newName *string) error {
	*newName = s.generator.GetFieldName(args)
	return nil
}

func StartNameServer(generator Generator) (string, error) {
	ns := &Server{generator: generator}

	nsServer := rpc.NewServer()
	if err := nsServer.Register(ns); err != nil {
		return "", err
	}

	listener, err := net.Listen("tcp4", "localhost:9191")
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
