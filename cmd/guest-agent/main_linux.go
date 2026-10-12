//go:build linux

package main

import (
	"flag"
	"log"
	"net"

	"github.com/mdlayher/vsock"
	"github.com/sailresearchco/saild-lite/api"
	"github.com/sailresearchco/saild-lite/internal/guest"
	"google.golang.org/grpc"
)

func main() {
	listen := flag.String("listen", "", "TCP listener instead of vsock (local testing)")
	flag.Parse()
	var lis net.Listener
	var err error
	if *listen == "" {
		lis, err = vsock.Listen(5000, nil)
	} else {
		lis, err = net.Listen("tcp", *listen)
	}
	if err != nil {
		log.Fatal(err)
	}
	s := guest.New("/run/sailbox-id")
	s.SetHostname = true
	defer s.Close()
	g := grpc.NewServer()
	api.RegisterGuestAgentServiceServer(g, s)
	api.RegisterSailboxExecServiceServer(g, s)
	log.Fatal(g.Serve(lis))
}
