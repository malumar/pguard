package main

import (
	"flag"
	"fmt"
	"github.com/glottis/inotify"
	"log"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	usersPath                   = "/sys/fs/cgroup/usery/"
	protocol                    = "unix"
	TestAddr                    = "/tmp/pguard.webserver.socket"
	ProdAddr                    = "/var/run/pguard.webserver.socket"
	cpuMaxStandard              = "50000 100000"
	cpuWeightStd                = "50"
	cpuMaxBusiness              = "70000 100000"
	cpuWeightBus                = "75"
	maxMemoryGb                 = 2
	connectionDeadLineInSeconds = 2

	defaultUid = 2003
	defaultGid = 2003
)

var (
	deleteAtRun  *bool
	removeSlices *bool
	uid          *int
	gid          *int
	started      = fmt.Sprintf("%d_", time.Now().UnixNano())
	counter      atomic.Uint64
	memoryMax    = strconv.FormatUint((1024*maxMemoryGb)*1024*1024, 10)
)

// main function initializes the flags and starts the server.
func main() {
	initializeFlags()
	setupWatcher()
	runServer()
}

func initializeFlags() {
	deleteAtRun = flag.Bool("delete", false, "Remove unused cgroups before startup")
	removeSlices = flag.Bool("removeSlices", false, fmt.Sprintf("Remove %s", usersPath))
	uid = flag.Int("uid", defaultUid, fmt.Sprintf("Set uid of %s (default %d)", usersPath, defaultUid))
	gid = flag.Int("gid", defaultGid, fmt.Sprintf("Set git of %s (default %d)", usersPath, defaultGid))
	flag.Parse()

	if *deleteAtRun {
		cleanupAllSubgroups(nil, "")
		if *removeSlices {
			os.Exit(0)
		}
	}
}

func setupWatcher() {
	watcher, err := inotify.NewWatcher()
	if err != nil {
		slog.Error("Failed to create watcher", "err", err)
		return
	}
	defer watcher.Close()
	defer cleanupAllSubgroups(watcher, "")

	go startCleaningCycle(watcher)
	go handleEvents(watcher)
}

func startCleaningCycle(watcher *inotify.Watcher) {
	for {
		slog.Info("Performing cyclic cleaning", "path", usersPath)
		cleanupAllSubgroups(watcher, "")
		time.Sleep(10 * time.Second)
	}
}

func handleEvents(watcher *inotify.Watcher) {
	for {
		select {
		case event := <-watcher.Events:
			handleEvent(event, watcher)
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			slog.Error("Watcher error", "err", err)
		}
	}
}

func handleEvent(event inotify.Event, watcher *inotify.Watcher) {
	if event.Op&inotify.Write == inotify.Write && !processExists(event.Name) {
		parentDir := filepath.Dir(event.Name)
		if strings.HasPrefix(parentDir, strings.TrimSuffix(usersPath, "/")) {
			if err := os.Remove(parentDir); err != nil {
				slog.Error("Failed to delete path", "err", err)
			}
		}
	}
}

func runServer() {
	addr := getSocketAddress()
	os.Mkdir(usersPath, 0755)
	setupCgroupConfig()

	listener, err := net.Listen(protocol, addr)
	if err != nil {
		log.Fatalf("Error starting server: %v", err)
	}

	if os.Getuid() == 0 {
		if err := os.Chown(addr, *uid, *gid); err != nil {
			slog.Error("can't chown addr path", "addr", addr, "err", err)
		}
	}

	defer listener.Close()

	slog.Info("Server launched", "address", addr)
	for {
		conn, err := listener.Accept()
		if err != nil {
			slog.Error("Failed to accept connection", "err", err)
			continue
		}
		go handleConnection(conn)
	}
}

func getSocketAddress() string {
	if os.Getuid() == 0 {
		return ProdAddr
	}
	return TestAddr
}

func setupCgroupConfig() {
	err := writeToFile("/sys/fs/cgroup/cgroup.subtree_control", "+cpu +io +memory +pids")
	if err != nil {
		log.Printf("Failed to write cgroup config: %v", err)
	}

	socketAddress := getSocketAddress()
	if _, err := os.Stat(socketAddress); err == nil {
		if err := os.RemoveAll(socketAddress); err != nil {
			log.Fatal(err)
		}
	}
}

func handleConnection(conn net.Conn) {
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(connectionDeadLineInSeconds * time.Second))

	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil {
		slog.Debug("Connection read error", "err", err)
		return
	}

	request := strings.TrimSpace(string(buf[:n]))
	args := strings.Split(request, "|")
	if len(args) != 3 {
		slog.Error("Expected 3 arguments in request", "args", args)
		return
	}

	if len(args[0]) == 0 {
		slog.Error("i expected pid", "arg", args[0])
		return

	}
	if len(args[1]) == 0 {
		slog.Error("i expected user", args[1])
		return
	}

	userSlice := fmt.Sprintf("%s/%s.slice/", usersPath, args[1])
	createCgroup(userSlice, args[2], args[0])
}

func createCgroup(slice, plan, pid string) {
	if err := CreateCgroupDir(slice, 0755); err != nil {
		slog.Error("Failed to create user slice", "path", slice, "err", err)
		return
	}

	cpuMax, cpuWeight := getPlanConfig(plan)
	subDir := fmt.Sprintf("%s%s_%d", slice, started, counter.Add(1))
	if err := CreateCgroupDir(subDir, 0755); err != nil {
		slog.Error("Failed to create user slice subdir", "path", subDir, "err", err)
		return
	}

	applyCgroupConfig(slice, subDir, cpuMax, cpuWeight, pid)
	slog.Info("Cgroup setup complete", "userSlice", slice, "subDir", subDir)
}

func applyCgroupConfig(slice, subDir, cpuMax, cpuWeight, pid string) {
	if err := writeToFile(slice+"cpu.max", "max"); err != nil {
		slog.Error("Failed to write cpu.max", "path", slice, "err", err)
	}
	if err := writeToFile(slice+"memory.max", memoryMax); err != nil {
		slog.Error("Failed to write memory.max", "path", slice, "err", err)
	}
	if err := writeToFile(subDir+"cpu.max", cpuMax); err != nil {
		slog.Error("Failed to write cpu.max", "path", subDir, "err", err)
	}
	if err := writeToFile(subDir+"cpu.weight", cpuWeight); err != nil {
		slog.Error("Failed to write cpu.weight", "path", subDir, "err", err)
	}
	if err := writeToFile(subDir+"cgroup.procs", pid); err != nil {
		slog.Error("Failed to write cgroup.procs", "path", subDir, "err", err)
	}
}

func getPlanConfig(plan string) (string, string) {
	switch strings.ToLower(plan) {
	case "business":
		return cpuMaxBusiness, cpuWeightBus
	default:
		return cpuMaxStandard, cpuWeightStd
	}
}

func cleanupAllSubgroups(watcher *inotify.Watcher, userSlice string) {
	dir := usersPath
	if userSlice != "" {
		dir = filepath.Join(usersPath, userSlice)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		slog.Error("Failed to read directory", "dir", dir, "err", err)
		return
	}

	for _, entry := range entries {
		if entry.IsDir() {
			cleanupSubgroup(filepath.Join(dir, entry.Name()), watcher)
		}
	}
}

func cleanupSubgroup(path string, watcher *inotify.Watcher) {
	if !processExists(filepath.Join(path, "cgroup.events")) {
		watcher.Remove(path)
		os.Remove(path)
	}
}

func processExists(file string) bool {
	content, err := os.ReadFile(file)
	if err != nil || len(content) < 21 {
		return false
	}
	for i := 10; i < len(content); i++ {
		if content[i] == 0x0a {
			// Do we have only one character and it's zero
			if i == 11 && content[i-1] == 0x30 {
				//fmt.Println("usun folder", filename)
				return false
			}
			break
		}
	}
	return true
}

func CreateCgroupDir(path string, mode os.FileMode) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return os.Mkdir(path, mode)
	}
	return nil
}

func writeToFile(path, data string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.WriteString(data)
	return err
}
