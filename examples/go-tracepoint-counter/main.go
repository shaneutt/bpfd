//go:build linux
// +build linux

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	gobpfd "github.com/redhat-et/bpfd/clients/gobpfd/v1"
	bpfdAppClient "github.com/redhat-et/bpfd/examples/pkg/bpfd-app-client"
	configMgmt "github.com/redhat-et/bpfd/examples/pkg/config-mgmt"
	"google.golang.org/grpc"
)

const (
	BpfProgramConfigName = "go-tracepoint-counter-example"
	BpfProgramMapIndex   = "tracepoint_stats_map"
	DefaultByteCodeFile  = "bpf_bpfel.o"
	DefaultConfigPath    = "/etc/bpfd/gocounter.toml"
	DefaultMapDir        = "/run/bpfd/fs/maps"
)

type Stats struct {
	Calls uint64
}

//go:generate bpf2go -cc clang -no-strip -cflags "-O2 -g -Wall" bpf ./bpf/tracepoint_counter.c -- -I.:/usr/include/bpf:/usr/include/linux
func main() {
	// pull the BPFD config management data to determine if we're running on a
	// system with BPFD available.
	paramData, err := configMgmt.ParseParamData(configMgmt.ProgTypeTracepoint, DefaultConfigPath, DefaultByteCodeFile)
	if err != nil {
		log.Printf("error processing parameters: %v\n", err)
		return
	}

	// determine the path to the tracepoint_stats_map, whether provided via CRD
	// or BPFD or otherwise.
	var mapPath string
	if paramData.CrdFlag { // get the map path from the API resource if on k8s
		mapPath, err = bpfdAppClient.GetMapPathDyn(BpfProgramConfigName, BpfProgramMapIndex)
		if err != nil {
			log.Fatalf("error reading BpfProgram CRD: %v\n", err)
		}
	} else { // if not on k8s, find the map path from the system

		// if the bytecode src is not a UUID provided by BPFD, we'll need to
		// load the program ourselves
		if paramData.BytecodeSrc != configMgmt.SrcUuid {
			cleanup, err := loadProgram(&paramData)
			if err != nil {
				log.Fatalf("failed to load BPF program: %v\n", err)
			}
			defer cleanup(paramData.Uuid)
		}

		mapPath = fmt.Sprintf("%s/%s/tracepoint_stats_map", DefaultMapDir, paramData.Uuid)
	}

	// load the pinned stats map which is keeping count of kill -SIGUSR1 calls
	opts := &ebpf.LoadPinOptions{
		ReadOnly:  false,
		WriteOnly: false,
		Flags:     0,
	}
	statsMap, err := ebpf.LoadPinnedMap(mapPath, opts)
	if err != nil {
		log.Printf("Failed to load pinned Map: %s\n", mapPath)
		log.Print(err)
		return
	}

	// send a SIGUSR1 signal to this program on repeat, which the BPF program
	// will report on to the stats map.
	go func() {
		for {
			syscall.Kill(os.Getpid(), syscall.SIGUSR1)
			time.Sleep(time.Second * 1)
		}
	}()

	// retrieve and report on the number of kill -SIGUSR1 calls
	index := 0
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		var stats []Stats
		var totalCalls uint64

		if err := statsMap.Lookup(&index, &stats); err != nil {
			log.Fatalf("map lookup failed: %v\n", err)
		}

		for _, stat := range stats {
			totalCalls += stat.Calls
		}

		log.Printf("%d SIGUSR1 signals seen\n", totalCalls)
	}
}

func loadProgram(paramData *configMgmt.ParameterData) (func(string), error) {
	// get the BPFD TLS credentials
	configFileData := configMgmt.LoadConfig(DefaultConfigPath)
	creds, err := configMgmt.LoadTLSCredentials(configFileData.Tls)
	if err != nil {
		return nil, err
	}

	// connect to the BPFD server
	ctx := context.Background()
	conn, err := grpc.DialContext(ctx, "localhost:50051", grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, err
	}
	c := gobpfd.NewLoaderClient(conn)

	// create a request to load the BPF program
	loadRequest := &gobpfd.LoadRequest{
		Location:    paramData.BytecodeLocation,
		SectionName: "stats",
		ProgramType: gobpfd.ProgramType_TRACEPOINT,
		AttachType:  &gobpfd.LoadRequest_SingleAttach{},
	}

	// send the load request to BPFD
	var res *gobpfd.LoadResponse
	res, err = c.Load(ctx, loadRequest)
	if err != nil {
		conn.Close()
		return nil, err
	}
	paramData.Uuid = res.GetId()
	log.Printf("program registered with %s id\n", paramData.Uuid)

	// provide a cleanup to unload the program
	return func(id string) {
		defer conn.Close()
		log.Printf("unloading program: %s\n", id)
		_, err = c.Unload(ctx, &gobpfd.UnloadRequest{Id: id})
		if err != nil {
			conn.Close()
			log.Fatalf("failed to unload program %s: %v", id, err)
		}
	}, nil
}
