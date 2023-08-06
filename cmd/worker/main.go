package main

import (
	pkg "Redacted-cloud-challenge/pkg/worker"
	"Redacted-cloud-challenge/proto"
	"flag"
	"fmt"
	"log"
	"net"

	"google.golang.org/grpc"
)

var (
	port = flag.Int("port", 7793, "The server port")
)

func main() {
	flag.Parse()
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("failed to listen %v at port %d", err, *port)
	}
	shutdownCh := make(chan struct{})
	defer close(shutdownCh)
	s := grpc.NewServer()
	proto.RegisterRedactedServer(s, pkg.NewWorkerServer(shutdownCh))
	log.Printf("Worker server listening at %v", l.Addr())
	if err := s.Serve(l); err != nil {
		log.Fatalf("Worked failed to serve %v", err)
	}
}
