package main

import (
	"Redacted-cloud-challenge/proto"
	"context"
	"flag"
	"time"

	log "github.com/sirupsen/logrus"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	// this is OK for dev - but definitely can't occur in a prod env
	addr string = "worker.Redacted.default.svc.cluster.local:7793"
)

func main() {
	flag.Parse()
	conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("Error dialing Redacted-worker: %v", err)
	}
	defer conn.Close()
	client := proto.NewRedactedClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	log.Printf("Creating Redacted cluster")
	// note: without WaitForReady here - the driver pod will fatal out with a lookup failure for ~3 restarts if the worker pod was also just launched
	// there seems to be a dns resolution backoff in the grpc-go package which the WaitForReady accounts for: https://github.com/grpc/grpc-go/issues/4650#issuecomment-895377308
	res, err := client.CreateRedacted(ctx, &proto.CreateRedactedRequest{OrgId: 2, RedactedClusterId: 3, Replicas: 30}, grpc.WaitForReady(true))
	if err != nil || res.Status == "ERROR" {
		log.Fatalf("Error creating Redacted cluster: %v", err)
	}
	log.Printf("Created Redacted cluster")
	log.Printf("Deleting Redacted cluster")
	deleteRes, err := client.DeleteRedacted(ctx, &proto.DeleteRedactedRequest{OrgId: 2, RedactedClusterId: 3})
	if err != nil || deleteRes.Status == "Error" {
		log.Fatalf("Error deleting Redacted cluster: %v", err)
	}
	log.Printf("Deleted Redacted cluster")
	// keep pod up for logging - there's other ways to do this, but for now - this works
	for {
	}
}
