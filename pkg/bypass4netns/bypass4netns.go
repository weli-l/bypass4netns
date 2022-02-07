package bypass4netns

// This code is copied from 'runc(https://github.com/opencontainers/runc/blob/v1.1.0/contrib/cmd/seccompagent/seccompagent.go)'
// The code is licensed under Apache-2.0 License

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"syscall"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/oraoto/go-pidfd"
	libseccomp "github.com/seccomp/libseccomp-golang"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

/*
#include <seccomp.h>
*/
import "C"

func closeStateFds(recvFds []int) {
	for i := range recvFds {
		unix.Close(i)
	}
}

// parseStateFds returns the seccomp-fd and closes the rest of the fds in recvFds.
// In case of error, no fd is closed.
// StateFds is assumed to be formatted as specs.ContainerProcessState.Fds and
// recvFds the corresponding list of received fds in the same SCM_RIGHT message.
func parseStateFds(stateFds []string, recvFds []int) (uintptr, error) {
	// Let's find the index in stateFds of the seccomp-fd.
	idx := -1
	err := false

	for i, name := range stateFds {
		if name == specs.SeccompFdName && idx == -1 {
			idx = i
			continue
		}

		// We found the seccompFdName twice. Error out!
		if name == specs.SeccompFdName && idx != -1 {
			err = true
		}
	}

	if idx == -1 || err {
		return 0, errors.New("seccomp fd not found or malformed containerProcessState.Fds")
	}

	if idx >= len(recvFds) || idx < 0 {
		return 0, errors.New("seccomp fd index out of range")
	}

	fd := uintptr(recvFds[idx])

	for i := range recvFds {
		if i == idx {
			continue
		}

		unix.Close(recvFds[i])
	}

	return fd, nil
}

// readProcMem read data from memory of specified pid process at the spcified offset.
func readProcMem(pid uint32, offset uint64, len uint64) ([]byte, error) {
	buffer := make([]byte, len) // PATH_MAX

	memfd, err := unix.Open(fmt.Sprintf("/proc/%d/mem", pid), unix.O_RDONLY, 0o777)
	if err != nil {
		return nil, err
	}
	defer unix.Close(memfd)

	size, err := unix.Pread(memfd, buffer, int64(offset))
	if err != nil {
		return nil, err
	}

	return buffer[:size], nil
}

func handleNewMessage(sockfd int) (uintptr, string, error) {
	const maxNameLen = 4096
	stateBuf := make([]byte, maxNameLen)
	oobSpace := unix.CmsgSpace(4)
	oob := make([]byte, oobSpace)

	n, oobn, _, _, err := unix.Recvmsg(sockfd, stateBuf, oob, 0)
	if err != nil {
		return 0, "", err
	}
	if n >= maxNameLen || oobn != oobSpace {
		return 0, "", fmt.Errorf("recvfd: incorrect number of bytes read (n=%d oobn=%d)", n, oobn)
	}

	// Truncate.
	stateBuf = stateBuf[:n]
	oob = oob[:oobn]

	scms, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return 0, "", err
	}
	if len(scms) != 1 {
		return 0, "", fmt.Errorf("recvfd: number of SCMs is not 1: %d", len(scms))
	}
	scm := scms[0]

	fds, err := unix.ParseUnixRights(&scm)
	if err != nil {
		return 0, "", err
	}

	containerProcessState := &specs.ContainerProcessState{}
	err = json.Unmarshal(stateBuf, containerProcessState)
	if err != nil {
		closeStateFds(fds)
		return 0, "", fmt.Errorf("cannot parse OCI state: %w", err)
	}

	fd, err := parseStateFds(containerProcessState.Fds, fds)
	if err != nil {
		closeStateFds(fds)
		return 0, "", err
	}

	return fd, containerProcessState.Metadata, nil
}

type context struct {
	notifFd libseccomp.ScmpFd
	req     *libseccomp.ScmpNotifReq
	resp    *libseccomp.ScmpNotifResp
}

// getFdInProcess get the file descriptor in other process
func getFdInProcess(pid, targetFd int) (int, error) {
	targetPidfd, err := pidfd.Open(int(pid), 0)
	if err != nil {
		return 0, fmt.Errorf("pidfd Open failed: %s", err)
	}
	defer syscall.Close(int(targetPidfd))

	fd, err := targetPidfd.GetFd(targetFd, 0)
	if err != nil {
		return 0, fmt.Errorf("pidfd GetFd failed: %s", err)
	}

	return fd, nil
}

// duplicateSocketOnHost duplicate socket in other process to socket on host.
// retun values are (duplicated socket fd, target socket fd in current process, error)
func duplicateSocketOnHost(ctx *context) (int, int, error) {
	sockfd, err := getFdInProcess(int(ctx.req.Pid), int(ctx.req.Data.Args[0]))
	if err != nil {
		return 0, 0, err
	}

	logrus.Debugf("got sockfd=%v", sockfd)
	sock_domain, err := syscall.GetsockoptInt(sockfd, syscall.SOL_SOCKET, syscall.SO_DOMAIN)
	if err != nil {
		return 0, 0, fmt.Errorf("getsockopt(SO_DOMAIN) failed: %s", err)
	}

	if sock_domain != syscall.AF_INET {
		return 0, 0, fmt.Errorf("expected AF_INET, got %d", sock_domain)
	}

	sock_type, err := syscall.GetsockoptInt(sockfd, syscall.SOL_SOCKET, syscall.SO_TYPE)
	if err != nil {
		return 0, 0, fmt.Errorf("getsockopt(SO_TYPE) failed: %s", err)
	}

	if sock_type != syscall.SOCK_STREAM {
		return 0, 0, fmt.Errorf("only SOCK_STREAM is supported")
	}

	sock_protocol, err := syscall.GetsockoptInt(sockfd, syscall.SOL_SOCKET, syscall.SO_PROTOCOL)
	if err != nil {
		return 0, 0, fmt.Errorf("getsockopt(SO_PROTOCOL) failed: %s", err)
	}

	sockfd2, err := syscall.Socket(sock_domain, sock_type, sock_protocol)
	if err != nil {
		return 0, 0, fmt.Errorf("socket failed: %s", err)
	}

	return sockfd2, sockfd, nil
}

// handleSysConnect handles syscall connect(2).
// If destination is outside of container network,
// it creates and configures a socket on host.
// Then, handler replaces container's socket to created one.
func (h *notifHandler) handleSysConnect(ctx *context) {
	addrlen := ctx.req.Data.Args[2]
	buf, err := readProcMem(ctx.req.Pid, ctx.req.Data.Args[1], addrlen)
	if err != nil {
		logrus.Errorf("Error readProcMem pid %v offset 0x%x: %s", ctx.req.Pid, ctx.req.Data.Args[1], err)
		return
	}

	addr := syscall.RawSockaddr{}
	reader := bytes.NewReader(buf)
	err = binary.Read(reader, binary.LittleEndian, &addr)
	if err != nil {
		logrus.Errorf("Error casting byte array to RawSocksddr: %s", err)
		return
	}

	if addr.Family != syscall.AF_INET {
		logrus.Debugf("Not AF_INET addr: %d", addr.Family)
		return
	}
	addrInet := syscall.RawSockaddrInet4{}
	reader.Seek(0, 0)
	err = binary.Read(reader, binary.LittleEndian, &addrInet)
	if err != nil {
		logrus.Errorf("Error casting byte array to RawSockaddrInet4: %s", err)
		return
	}

	logrus.Debugf("%v", addrInet)
	port := ((addrInet.Port & 0xFF) << 8) | (addrInet.Port >> 8)
	logrus.Infof("connect(pid=%d): sockfd=%d, port=%d, ip=%v", ctx.req.Pid, ctx.req.Data.Args[0], port, addrInet.Addr)

	// TODO: more sophisticated way to convert.
	ipAddr := net.IPv4(addrInet.Addr[0], addrInet.Addr[1], addrInet.Addr[2], addrInet.Addr[3])
	destIsIgnored := h.isIgnored(ipAddr)

	key := fmt.Sprintf("%d:%d", ctx.req.Pid, ctx.req.Data.Args[0])
	sockStatus, ok := h.socketInfo.status[key]
	var sockfd int
	var sockfd2 int
	if !ok {
		if destIsIgnored {
			// the socket has never been bypassed and no need to bypass
			logrus.Infof("%s is ignored, skipping.", ipAddr.String())
			logrus.Debug("the socket has never been bypassed and no need to bypass")
			return
		} else {
			// the socket has never been bypassed and need to bypass
			logrus.Debug("the socket has never been bypassed and need to bypass")
			sockfd2, sockfd, err = duplicateSocketOnHost(ctx)
			if err != nil {
				logrus.Errorf("duplicating socket failed: %s", err)
				return
			}

			err = h.socketInfo.configureSocket(ctx, sockfd2)
			if err != nil {
				syscall.Close(sockfd2)
				logrus.Errorf("setsocketoptions failed: %s", err)
				return
			}

			h.socketInfo.status[key] = socketStatus{
				state:     Bypassed,
				fdInNetns: sockfd,
				fdInHost:  sockfd2,
			}
		}
	} else {
		if sockStatus.state == Bypassed {
			if !destIsIgnored {
				// the socket has been bypassed and continue to be bypassed
				logrus.Debug("the socket has been bypassed and continue to be bypassed")
				return
			} else {
				// the socket has been bypassed and need to switch back to socket in netns
				logrus.Debugf("the socket has been bypassed and need to switch back to socket in netns(%d -> %d)", sockStatus.fdInHost, sockStatus.fdInNetns)
				sockStatus.state = SwitchBacked
				sockfd2 = sockStatus.fdInNetns

				h.socketInfo.status[key] = sockStatus
			}
		} else if sockStatus.state == SwitchBacked {
			if destIsIgnored {
				// the socket has been switchbacked(not bypassed) and no need to be bypassed
				logrus.Debug("the socket has been switchbacked(not bypassed) and no need to be bypassed")
				return
			} else {
				// the socket has been switchbacked(not bypassed) and need to bypass again
				logrus.Debugf("the socket has been switchbacked(not bypassed) and need bypass again(%d -> %d)", sockStatus.fdInNetns, sockStatus.fdInHost)
				sockStatus.state = Bypassed
				sockfd2 = sockStatus.fdInHost

				h.socketInfo.status[key] = sockStatus
			}
		} else {
			panic(fmt.Errorf("unexpected state :%d", sockStatus.state))
		}
	}

	addfd := seccompNotifAddFd{
		id:         ctx.req.ID,
		flags:      C.SECCOMP_ADDFD_FLAG_SETFD,
		srcfd:      uint32(sockfd2),
		newfd:      uint32(ctx.req.Data.Args[0]),
		newfdFlags: 0,
	}

	err = addfd.ioctlNotifAddFd(ctx.notifFd)
	if err != nil {
		logrus.Errorf("ioctl NotifAddFd failed: %s", err)
		return
	}
}

// handleSysBind handles syscall bind(2).
// If binding port is the target of port-forwarding,
// it creates and configures including bind(2) a socket on host.
// Then, handler replaces container's socket to created one.
func (h *notifHandler) handleSysBind(ctx *context) {
	addrlen := ctx.req.Data.Args[2]
	buf, err := readProcMem(ctx.req.Pid, ctx.req.Data.Args[1], addrlen)
	if err != nil {
		logrus.Errorf("Error readProcMem pid %v offset 0x%x: %s", ctx.req.Pid, ctx.req.Data.Args[1], err)
		return
	}

	addr := syscall.RawSockaddr{}
	reader := bytes.NewReader(buf)
	err = binary.Read(reader, binary.LittleEndian, &addr)
	if err != nil {
		logrus.Errorf("Error casting byte array to RawSocksddr: %s", err)
		return
	}

	if addr.Family != syscall.AF_INET {
		logrus.Debugf("Not AF_INET addr: %d", addr.Family)
		return
	}
	addrInet := syscall.RawSockaddrInet4{}
	reader.Seek(0, 0)
	err = binary.Read(reader, binary.LittleEndian, &addrInet)
	if err != nil {
		logrus.Errorf("Error casting byte array to RawSockaddrInet4: %s", err)
		return
	}

	logrus.Debugf("%v", addrInet)
	port := ((addrInet.Port & 0xFF) << 8) | (addrInet.Port >> 8)
	logrus.Infof("bind(pid=%d): sockfd=%d, port=%d, ip=%v", ctx.req.Pid, ctx.req.Data.Args[0], port, addrInet.Addr)

	// TODO: get port-fowrad mapping from nerdctl
	if port != 5201 {
		logrus.Infof("not mapped port=%d", port)
		return
	}

	sockfd2, sockfd, err := duplicateSocketOnHost(ctx)
	if err != nil {
		logrus.Errorf("duplicating socket failed: %s", err)
		return
	}
	defer syscall.Close(sockfd)
	defer syscall.Close(sockfd2)

	err = h.socketInfo.configureSocket(ctx, sockfd2)
	if err != nil {
		syscall.Close(sockfd2)
		logrus.Errorf("setsocketoptions failed: %s", err)
		return
	}

	bind_addr := syscall.SockaddrInet4{
		Port: int(8080),
		Addr: addrInet.Addr,
	}

	err = syscall.Bind(sockfd2, &bind_addr)
	if err != nil {
		logrus.Errorf("bind failed: %s", err)
		return
	}

	addfd := seccompNotifAddFd{
		id:         ctx.req.ID,
		flags:      C.SECCOMP_ADDFD_FLAG_SETFD,
		srcfd:      uint32(sockfd2),
		newfd:      uint32(ctx.req.Data.Args[0]),
		newfdFlags: 0,
	}

	err = addfd.ioctlNotifAddFd(ctx.notifFd)
	if err != nil {
		logrus.Errorf("ioctl NotifAddFd failed: %s", err)
		return
	}

	ctx.resp.Flags &= (^uint32(C.SECCOMP_USER_NOTIF_FLAG_CONTINUE))
}

// handleSyssetsockopt handles `setsockopt(2)` and records options.
// Recorded options are used in `handleSysConnect` or `handleSysBind` via `setSocketoptions` to configure created sockets.
func (h *notifHandler) handleSysSetsockopt(ctx *context) {
	logrus.Debugf("setsockopt(pid=%d): sockfd=%d", ctx.req.Pid, ctx.req.Data.Args[0])
	err := h.socketInfo.recordSocketOption(ctx)
	if err != nil {
		logrus.Errorf("recordSocketOption failed: %s", err)
	}
}

// handleSysClose handles `close(2)` and delete recorded socket options.
func (h *notifHandler) handleSysClose(ctx *context) {
	sockfd := ctx.req.Data.Args[0]
	logrus.Debugf("close(pid=%d): sockfd=%d", ctx.req.Pid, sockfd)
	h.socketInfo.deleteSocket(ctx)
}

// handleReq handles seccomp notif requests and configures responses.
func (h *notifHandler) handleReq(ctx *context) {
	syscallName, err := ctx.req.Data.Syscall.GetName()
	if err != nil {
		logrus.Errorf("Error decoding syscall %v(): %s", ctx.req.Data.Syscall, err)
		// TODO: error handle
		return
	}
	logrus.Debugf("Received syscall %q, pid %v, arch %q, args %+v", syscallName, ctx.req.Pid, ctx.req.Data.Arch, ctx.req.Data.Args)

	ctx.resp.Flags |= C.SECCOMP_USER_NOTIF_FLAG_CONTINUE

	switch syscallName {
	case "connect":
		h.handleSysConnect(ctx)
	case "bind":
		h.handleSysBind(ctx)
	case "setsockopt":
		h.handleSysSetsockopt(ctx)
	case "close":
		// handling close(2) may cause performance degradation
		h.handleSysClose(ctx)
	default:
		logrus.Errorf("Unknown syscall %q", syscallName)
		// TODO: error handle
		return
	}

}

// notifHandler handles seccomp notifications and response to them.
func (h *notifHandler) handle() {
	defer unix.Close(int(h.fd))

	for {
		req, err := libseccomp.NotifReceive(h.fd)
		if err != nil {
			logrus.Errorf("Error in NotifReceive(): %s", err)
			continue
		}

		ctx := context{
			notifFd: h.fd,
			req:     req,
			resp: &libseccomp.ScmpNotifResp{
				ID:    req.ID,
				Error: 0,
				Val:   0,
				Flags: libseccomp.NotifRespFlagContinue,
			},
		}

		// TOCTOU check
		if err := libseccomp.NotifIDValid(h.fd, req.ID); err != nil {
			logrus.Errorf("TOCTOU check failed: req.ID is no longer valid: %s", err)
			continue
		}

		h.handleReq(&ctx)

		if err = libseccomp.NotifRespond(h.fd, ctx.resp); err != nil {
			logrus.Errorf("Error in notification response: %s", err)
			continue
		}
	}
}

type Handler struct {
	socketPath     string
	ignoredSubnets []net.IPNet
}

// NewHandler creates new seccomp notif handler
func NewHandler(socketPath string) *Handler {
	handler := Handler{
		socketPath:     socketPath,
		ignoredSubnets: []net.IPNet{},
	}

	return &handler
}

// SetIgnoreSubnets configures subnets to ignore in bypass4netns.
func (h *Handler) SetIgnoredSubnets(subnets []net.IPNet) {
	h.ignoredSubnets = subnets
}

type notifHandler struct {
	fd             libseccomp.ScmpFd
	ignoredSubnets []net.IPNet
	socketInfo     socketInfo
}

func (h *Handler) newNotifHandler(fd uintptr) *notifHandler {
	notifHandler := notifHandler{
		fd: libseccomp.ScmpFd(fd),
		socketInfo: socketInfo{
			options: map[string][]socketOption{},
			status:  map[string]socketStatus{},
		},
	}
	notifHandler.ignoredSubnets = make([]net.IPNet, len(h.ignoredSubnets))
	// Deep copy []net.IPNet because each thread accesses it.
	copy(notifHandler.ignoredSubnets, h.ignoredSubnets)
	return &notifHandler
}

// isIgnored checks the IP address is ignored.
func (h *notifHandler) isIgnored(ip net.IP) bool {
	for _, subnet := range h.ignoredSubnets {
		if subnet.Contains(ip) {
			return true
		}
	}

	return false
}

// StartHandle starts seccomp notif handler
func (h *Handler) StartHandle() {
	logrus.Info("Waiting for seccomp file descriptors")
	l, err := net.Listen("unix", h.socketPath)
	if err != nil {
		logrus.Fatalf("Cannot listen: %w", err)
	}
	defer l.Close()

	for {
		conn, err := l.Accept()
		if err != nil {
			logrus.Errorf("Cannot accept connection: %s", err)
			continue
		}
		socket, err := conn.(*net.UnixConn).File()
		conn.Close()
		if err != nil {
			logrus.Errorf("Cannot get socket: %v", err)
			continue
		}
		newFd, _, err := handleNewMessage(int(socket.Fd()))
		socket.Close()
		if err != nil {
			logrus.Errorf("Error receiving seccomp file descriptor: %v", err)
			continue
		}

		logrus.Infof("Received new seccomp fd: %v", newFd)
		notifHandler := h.newNotifHandler(newFd)
		go notifHandler.handle()
	}
}
