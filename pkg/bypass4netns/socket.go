package bypass4netns

import (
	gocontext "context"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

type socketOption struct {
	level   uint64
	optname uint64
	optval  []byte
	optlen  uint64
}

// Handle F_SETFL, F_SETFD options
type fcntlOption struct {
	cmd   uint64
	value uint64
}

type socketState int

const (
	// NotBypassableSocket  means that the fd is not socket or not bypassed
	NotBypassable socketState = iota

	// NotBypassed means that the socket is not bypassed.
	NotBypassed

	// Bypassed means that the socket is replaced by one created on the host
	Bypassed

	// Error happened after bypass. Nothing can be done to recover from this state.
	Error
)

func (ss socketState) String() string {
	switch ss {
	case NotBypassable:
		return "NotBypassable"
	case NotBypassed:
		return "NotBypassed"
	case Bypassed:
		return "Bypassed"
	case Error:
		return "Error"
	default:
		panic(fmt.Sprintf("unexpected enum %d: String() is not implmented", ss))
	}
}

type processStatus struct {
	sockets map[int]*socketStatus
}

func newProcessStatus() *processStatus {
	return &processStatus{
		sockets: map[int]*socketStatus{},
	}
}

type socketStatus struct {
	state      socketState
	pid        int
	sockfd     int
	sockDomain int
	sockType   int
	sockProto  int
	// address for bind or connect
	addr          *sockaddr
	socketOptions []socketOption
	fcntlOptions  []fcntlOption

	logger     *logrus.Entry
	ignoreBind bool
}

func newSocketStatus(pid int, sockfd int, sockDomain, sockType, sockProto int, ignoreBind bool) *socketStatus {
	return &socketStatus{
		state:         NotBypassed,
		pid:           pid,
		sockfd:        sockfd,
		sockDomain:    sockDomain,
		sockType:      sockType,
		sockProto:     sockProto,
		socketOptions: []socketOption{},
		fcntlOptions:  []fcntlOption{},
		logger:        logrus.WithFields(logrus.Fields{"pid": pid, "sockfd": sockfd}),
		ignoreBind:    ignoreBind,
	}
}

func (ss *socketStatus) handleSysSetsockopt(pid int, handler *notifHandler, ctx *context) {
	ss.logger.Debug("handle setsockopt")
	level := ctx.req.Data.Args[1]
	optname := ctx.req.Data.Args[2]
	optlen := ctx.req.Data.Args[4]
	optval, err := handler.readProcMem(pid, ctx.req.Data.Args[3], optlen)
	if err != nil {
		ss.logger.Errorf("setsockopt readProcMem failed pid %v offset 0x%x: %s", pid, ctx.req.Data.Args[1], err)
	}

	value := socketOption{
		level:   level,
		optname: optname,
		optval:  optval,
		optlen:  optlen,
	}
	ss.socketOptions = append(ss.socketOptions, value)

	ss.logger.Debugf("setsockopt level=%d optname=%d optval=%v optlen=%d was recorded.", level, optname, optval, optlen)
}

func (ss *socketStatus) handleSysFcntl(ctx *context) {
	ss.logger.Debug("handle fcntl")
	fcntlCmd := ctx.req.Data.Args[1]
	switch fcntlCmd {
	case unix.F_SETFD: // 0x2
	case unix.F_SETFL: // 0x4
		opt := fcntlOption{
			cmd:   fcntlCmd,
			value: ctx.req.Data.Args[2],
		}
		ss.fcntlOptions = append(ss.fcntlOptions, opt)
		ss.logger.Debugf("fcntl cmd=0x%x value=%d was recorded.", fcntlCmd, opt.value)
	case unix.F_GETFL: // 0x3
		// ignore these
	default:
		ss.logger.Warnf("Unknown fcntl command 0x%x ignored.", fcntlCmd)
	}
}

func (ss *socketStatus) handleSysConnect(handler *notifHandler, ctx *context) {
	destAddr, err := handler.readSockaddrFromProcess(ss.pid, ctx.req.Data.Args[1], ctx.req.Data.Args[2])
	if err != nil {
		ss.logger.Errorf("failed to read sockaddr from process: %q", err)
		return
	}
	ss.addr = destAddr

	if handler.ip != "" && destAddr.IP.String() != handler.ip {
		ss.logger.Infof("destination IP %s does not match handler IP %s, skipping socket creation", destAddr.IP, handler.ip)
		ss.state = NotBypassable
		return
	}

	var newDestAddr net.IP
	switch destAddr.Family {
	case syscall.AF_INET:
		newDestAddr = net.IPv4zero
		newDestAddr = newDestAddr.To4()
		newDestAddr[0] = 127
		newDestAddr[3] = 1
	case syscall.AF_INET6:
		newDestAddr = net.IPv6loopback
		newDestAddr = newDestAddr.To16()
	default:
		ss.logger.Errorf("unexpected destination address family %d", destAddr.Family)
		ss.state = Error
		return
	}

	// check whether the destination is bypassed or not.
	connectToLoopback := false
	connectToInterface := false
	connectToOtherBypassedContainer := false
	var fwdPort ForwardPortMapping
	if !ss.ignoreBind {
		var ok bool
		fwdPort, ok = handler.forwardingPorts[int(destAddr.Port)]
		if ok {
			if destAddr.IP.IsLoopback() {
				ss.logger.Infof("destination address %v is loopback and bypassed", destAddr)
				connectToLoopback = true
			} else if contIf, ok := handler.containerInterfaces[destAddr.String()]; ok && contIf.containerID == handler.state.State.ID {
				ss.logger.Infof("destination address %v is interface's address and bypassed", destAddr)
				connectToInterface = true
			}
		}
	}

	if handler.multinode.Enable && destAddr.IP.IsPrivate() {
		// currently, only private addresses are available in multinode communication.
		key := ETCD_MULTINODE_PREFIX + destAddr.String()
		ctx, cancel := gocontext.WithTimeout(gocontext.Background(), 2*time.Second)
		res, err := handler.multinode.etcdClient.Get(ctx, ETCD_MULTINODE_PREFIX+destAddr.String())
		cancel()
		if err != nil {
			ss.logger.WithError(err).Warnf("destination address %q is not registered", key)
		} else {
			if len(res.Kvs) != 1 {
				ss.logger.Errorf("invalid len(res.Kvs) %d", len(res.Kvs))
				ss.state = Error
				return
			}
			hostAddrWithPort := string(res.Kvs[0].Value)
			hostAddrs := strings.Split(hostAddrWithPort, ":")
			if len(hostAddrs) != 2 {
				ss.logger.Errorf("invalid address format %q", hostAddrWithPort)
				ss.state = Error
				return
			}
			hostAddr := hostAddrs[0]
			hostPort, err := strconv.Atoi(hostAddrs[1])
			if err != nil {
				ss.logger.Errorf("invalid address format %q", hostAddrWithPort)
				ss.state = Error
				return
			}
			newDestAddr = net.ParseIP(hostAddr)
			fwdPort.HostPort = hostPort
			connectToOtherBypassedContainer = true
			ss.logger.Infof("destination address %v is container address and bypassed via overlay network", destAddr)
		}
	} else if handler.c2cConnections.Enable {
		contIf, ok := handler.containerInterfaces[destAddr.String()]
		if ok {
			ss.logger.Infof("destination address %v is container address and bypassed", destAddr)
			fwdPort.HostPort = contIf.hostPort
			connectToOtherBypassedContainer = true
		}
	}

	// check whether the destination container socket is bypassed or not.
	isNotBypassed := handler.nonBypassable.Contains(destAddr.IP)

	if !connectToLoopback && !connectToInterface && !connectToOtherBypassedContainer && isNotBypassed {
		ss.logger.Infof("destination address %v is not bypassed.", destAddr.IP)
		ss.state = NotBypassable
		return
	}

	sockfdOnHost, err := syscall.Socket(ss.sockDomain, ss.sockType, ss.sockProto)
	if err != nil {
		ss.logger.Errorf("failed to create socket: %q", err)
		ss.state = NotBypassable
		return
	}
	defer syscall.Close(sockfdOnHost)

	err = ss.configureSocket(sockfdOnHost)
	if err != nil {
		ss.logger.Errorf("failed to configure socket: %q", err)
		ss.state = NotBypassable
		return
	}

	addfd := seccompNotifAddFd{
		id:    ctx.req.ID,
		flags: SeccompAddFdFlagSetFd,
		srcfd: uint32(sockfdOnHost),
		newfd: uint32(ctx.req.Data.Args[0]),
		// SOCK_CLOEXEC must be configured in this flag.
		newfdFlags: uint32(ss.sockType & syscall.SOCK_CLOEXEC),
	}

	err = addfd.ioctlNotifAddFd(ctx.notifFd)
	if err != nil {
		ss.logger.Errorf("ioctl NotifAddFd failed: %q", err)
		ss.state = NotBypassable
		return
	}

	if connectToLoopback || connectToInterface || connectToOtherBypassedContainer {
		p := make([]byte, 2)
		binary.BigEndian.PutUint16(p, uint16(fwdPort.HostPort))
		// writing host port at sock_addr's port offset
		// TODO: should we return dummy value when getpeername(2) is called?
		err = handler.writeProcMem(ss.pid, ctx.req.Data.Args[1]+2, p)
		if err != nil {
			ss.logger.Errorf("failed to rewrite destination port: %q", err)
			ss.state = Error
			return
		}
		ss.logger.Infof("destination's port %d is rewritten to host-side port %d", ss.addr.Port, fwdPort.HostPort)
	}

	if connectToInterface || connectToOtherBypassedContainer {
		// writing host's loopback address to connect to bypassed socket at sock_addr's address offset
		// TODO: should we return dummy value when getpeername(2) is called?
		switch destAddr.Family {
		case syscall.AF_INET:
			newDestAddr = newDestAddr.To4()
			err = handler.writeProcMem(ss.pid, ctx.req.Data.Args[1]+4, newDestAddr[0:4])
		case syscall.AF_INET6:
			newDestAddr = newDestAddr.To16()
			err = handler.writeProcMem(ss.pid, ctx.req.Data.Args[1]+8, newDestAddr[0:16])
		default:
			ss.logger.Errorf("unexpected destination address family %d", destAddr.Family)
			ss.state = Error
			return
		}
		if err != nil {
			ss.logger.Errorf("failed to rewrite destination address: %q", err)
			ss.state = Error
			return
		}

		ss.logger.Infof("destination address %s is rewritten to %s", destAddr.IP, newDestAddr)
	}

	ss.state = Bypassed
	ss.logger.Infof("bypassed connect socket destAddr=%s", ss.addr)
}

func (ss *socketStatus) handleSysBind(pid int, handler *notifHandler, ctx *context) {
	if ss.ignoreBind {
		ss.state = NotBypassable
		return
	}
	sa, err := handler.readSockaddrFromProcess(pid, ctx.req.Data.Args[1], ctx.req.Data.Args[2])
	if err != nil {
		ss.logger.Errorf("failed to read sockaddr from process: %q", err)
		ss.state = NotBypassable
		return
	}
	ss.addr = sa

	ss.logger.Infof("handle port=%d, ip=%v", sa.Port, sa.IP)

	// TODO: get port-fowrad mapping from nerdctl
	fwdPort, ok := handler.forwardingPorts[int(sa.Port)]
	if !ok {
		ss.logger.Infof("port=%d is not target of port forwarding.", sa.Port)
		ss.state = NotBypassable
		return
	}

	sockfdOnHost, err := syscall.Socket(ss.sockDomain, ss.sockType, ss.sockProto)
	if err != nil {
		ss.logger.Errorf("failed to create socket: %q", err)
		ss.state = NotBypassable
		return
	}
	defer syscall.Close(sockfdOnHost)

	err = ss.configureSocket(sockfdOnHost)
	if err != nil {
		ss.logger.Errorf("failed to configure socket: %q", err)
		ss.state = NotBypassable
		return
	}

	var bind_addr syscall.Sockaddr

	switch sa.Family {
	case syscall.AF_INET:
		var addr [4]byte
		for i := 0; i < 4; i++ {
			addr[i] = sa.IP[i]
		}
		bind_addr = &syscall.SockaddrInet4{
			Port: fwdPort.HostPort,
			Addr: addr,
		}
	case syscall.AF_INET6:
		var addr [16]byte
		for i := 0; i < 16; i++ {
			addr[i] = sa.IP[i]
		}
		bind_addr = &syscall.SockaddrInet6{
			Port:   fwdPort.HostPort,
			ZoneId: sa.ScopeID,
			Addr:   addr,
		}
	}

	err = syscall.Bind(sockfdOnHost, bind_addr)
	if err != nil {
		ss.logger.Errorf("bind failed: %s", err)
		ss.state = NotBypassable
		return
	}

	addfd := seccompNotifAddFd{
		id:    ctx.req.ID,
		flags: SeccompAddFdFlagSetFd,
		srcfd: uint32(sockfdOnHost),
		newfd: uint32(ctx.req.Data.Args[0]),
		// SOCK_CLOEXEC must be configured in this flag.
		newfdFlags: uint32(ss.sockType & syscall.SOCK_CLOEXEC),
	}

	err = addfd.ioctlNotifAddFd(ctx.notifFd)
	if err != nil {
		ss.logger.Errorf("ioctl NotifAddFd failed: %s", err)
		ss.state = NotBypassable
		return
	}

	ss.state = Bypassed
	ss.logger.Infof("bypassed bind socket for %d:%d is done", fwdPort.HostPort, fwdPort.ChildPort)

	ctx.resp.Flags &= (^uint32(SeccompUserNotifFlagContinue))
}

func (ss *socketStatus) handleSysGetpeername(handler *notifHandler, ctx *context) {
	if ss.addr == nil {
		return
	}

	buf, err := ss.addr.toBytes()
	if err != nil {
		ss.logger.WithError(err).Errorf("failed to serialize address %s", ss.addr)
		return
	}

	err = handler.writeProcMem(ss.pid, ctx.req.Data.Args[1], buf)
	if err != nil {
		ss.logger.WithError(err).Errorf("failed to write address %s", ss.addr)
		return
	}

	bufLen := make([]byte, 4)
	binary.LittleEndian.PutUint32(bufLen, uint32(len(buf)))
	err = handler.writeProcMem(ss.pid, ctx.req.Data.Args[2], bufLen)
	if err != nil {
		ss.logger.WithError(err).Errorf("failed to write address length %d", len(buf))
		return
	}

	ctx.resp.Flags &= (^uint32(SeccompUserNotifFlagContinue))

	ss.logger.Infof("rewrite getpeername() address to %s", ss.addr)
}

func (ss *socketStatus) configureSocket(sockfd int) error {
	for _, optVal := range ss.socketOptions {
		_, _, errno := syscall.Syscall6(syscall.SYS_SETSOCKOPT, uintptr(sockfd), uintptr(optVal.level), uintptr(optVal.optname), uintptr(unsafe.Pointer(&optVal.optval[0])), uintptr(optVal.optlen), 0)
		if errno != 0 {
			return fmt.Errorf("setsockopt failed(%v): %s", optVal, errno)
		}
		ss.logger.Debugf("configured socket option val=%v", optVal)
	}

	for _, fcntlVal := range ss.fcntlOptions {
		_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(sockfd), uintptr(fcntlVal.cmd), uintptr(fcntlVal.value))
		if errno != 0 {
			return fmt.Errorf("fnctl failed(%v): %s", fcntlVal, errno)
		}
		ss.logger.Debugf("configured socket fcntl val=%v", fcntlVal)
	}

	return nil
}
