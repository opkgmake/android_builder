package main

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	maxPacketSize   = 1 << 16
	tcpHeaderLength = 4
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	mode := flag.String("mode", "", "Operating mode: client or server")
	udpListen := flag.String("udp-listen", "127.0.0.1:51820", "UDP listen address (client) or UDP bind address (server)")
	tcpAddress := flag.String("tcp", "", "TCP server address (client) or listen address (server)")
	udpTarget := flag.String("udp-target", "", "Server mode: WireGuard endpoint to forward packets to")
	reconnectDelay := flag.Duration("reconnect", 5*time.Second, "Client mode: delay before attempting to reconnect")
	readTimeout := flag.Duration("read-timeout", 5*time.Second, "Per-read timeout for UDP sockets")
	flag.Parse()

	if *mode != "client" && *mode != "server" {
		log.Fatalf("mode must be 'client' or 'server'")
	}

	if *tcpAddress == "" {
		log.Fatalf("tcp address is required")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	switch *mode {
	case "client":
		if err := runClient(ctx, *udpListen, *tcpAddress, *reconnectDelay, *readTimeout); err != nil && !errors.Is(err, context.Canceled) {
			log.Fatalf("client terminated: %v", err)
		}
	case "server":
		if *udpTarget == "" {
			log.Fatalf("server mode requires --udp-target")
		}
		if err := runServer(ctx, *udpListen, *udpTarget, *tcpAddress, *readTimeout); err != nil && !errors.Is(err, context.Canceled) {
			log.Fatalf("server terminated: %v", err)
		}
	}
}

func runClient(ctx context.Context, udpListen, tcpAddress string, reconnectDelay, readTimeout time.Duration) error {
	udpAddr, err := net.ResolveUDPAddr("udp", udpListen)
	if err != nil {
		return fmt.Errorf("resolve udp listen: %w", err)
	}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("listen udp: %w", err)
	}
	defer udpConn.Close()

	log.Printf("client: listening for WireGuard packets on %s", udpConn.LocalAddr())

	var peer atomic.Value
	peer.Store((*net.UDPAddr)(nil))

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Printf("client: dialing TCP %s", tcpAddress)
		tcpConn, err := net.Dial("tcp", tcpAddress)
		if err != nil {
			log.Printf("client: dial failed: %v", err)
			select {
			case <-time.After(reconnectDelay):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		log.Printf("client: connected to %s", tcpConn.RemoteAddr())
		errCh := make(chan error, 2)

		go func() {
			errCh <- forwardUDPToTCP(ctx, udpConn, tcpConn, &peer, readTimeout)
		}()

		go func() {
			errCh <- forwardTCPToUDP(ctx, tcpConn, udpConn, &peer)
		}()

		err = <-errCh
		tcpConn.Close()

		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			log.Printf("client: connection closed: %v", err)
		} else {
			log.Printf("client: connection closed")
		}

		select {
		case <-time.After(reconnectDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func runServer(ctx context.Context, udpBind, udpTarget, tcpListen string, readTimeout time.Duration) error {
	udpBindAddr, err := net.ResolveUDPAddr("udp", udpBind)
	if err != nil {
		return fmt.Errorf("resolve udp bind: %w", err)
	}
	targetAddr, err := net.ResolveUDPAddr("udp", udpTarget)
	if err != nil {
		return fmt.Errorf("resolve udp target: %w", err)
	}

	tcpLn, err := net.Listen("tcp", tcpListen)
	if err != nil {
		return fmt.Errorf("listen tcp: %w", err)
	}
	defer tcpLn.Close()

	log.Printf("server: listening for TCP connections on %s", tcpLn.Addr())

	for {
		tcpConn, err := tcpLn.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				log.Printf("server: accept temporary error: %v", err)
				continue
			}
			return fmt.Errorf("accept tcp: %w", err)
		}

		log.Printf("server: accepted connection from %s", tcpConn.RemoteAddr())
		go func(conn net.Conn) {
			if err := handleServerConnection(ctx, conn, udpBindAddr, targetAddr, readTimeout); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("server: connection %s terminated: %v", conn.RemoteAddr(), err)
			}
		}(tcpConn)
	}
}

func handleServerConnection(ctx context.Context, tcpConn net.Conn, udpBind, udpTarget *net.UDPAddr, readTimeout time.Duration) error {
	defer tcpConn.Close()

	udpConn, err := net.ListenUDP("udp", udpBind)
	if err != nil {
		return fmt.Errorf("listen udp: %w", err)
	}
	defer udpConn.Close()

	log.Printf("server: UDP socket bound on %s forwarding to %s", udpConn.LocalAddr(), udpTarget)

	errCh := make(chan error, 2)
	go func() {
		errCh <- forwardTCPFramesToUDP(ctx, tcpConn, udpConn, udpTarget)
	}()
	go func() {
		errCh <- forwardUDPResponsesToTCP(ctx, udpConn, tcpConn, udpTarget, readTimeout)
	}()

	return <-errCh
}

func forwardUDPToTCP(ctx context.Context, udpConn *net.UDPConn, tcpConn net.Conn, peer *atomic.Value, readTimeout time.Duration) error {
	buf := make([]byte, maxPacketSize)
	for {
		if err := udpConn.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
			return err
		}
		n, addr, err := udpConn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
					continue
				}
			}
			return err
		}
		peer.Store(addr)
		if err := writeFrame(tcpConn, buf[:n]); err != nil {
			return err
		}
	}
}

func forwardTCPToUDP(ctx context.Context, tcpConn net.Conn, udpConn *net.UDPConn, peer *atomic.Value) error {
	for {
		payload, err := readFrame(tcpConn)
		if err != nil {
			return err
		}
		p := peer.Load().(*net.UDPAddr)
		if p == nil {
			log.Printf("client: dropping TCP packet because no UDP peer is known yet")
			continue
		}
		if _, err := udpConn.WriteToUDP(payload, p); err != nil {
			return err
		}
	}
}

func forwardTCPFramesToUDP(ctx context.Context, tcpConn net.Conn, udpConn *net.UDPConn, target *net.UDPAddr) error {
	for {
		payload, err := readFrame(tcpConn)
		if err != nil {
			return err
		}
		if _, err := udpConn.WriteToUDP(payload, target); err != nil {
			return err
		}
	}
}

func forwardUDPResponsesToTCP(ctx context.Context, udpConn *net.UDPConn, tcpConn net.Conn, target *net.UDPAddr, readTimeout time.Duration) error {
	buf := make([]byte, maxPacketSize)
	for {
		if err := udpConn.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
			return err
		}
		n, addr, err := udpConn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
					continue
				}
			}
			return err
		}
		if !udpAddrsEqual(addr, target) {
			log.Printf("server: ignoring packet from unexpected UDP peer %s", addr)
			continue
		}
		if err := writeFrame(tcpConn, buf[:n]); err != nil {
			return err
		}
	}
}

func writeFrame(w io.Writer, payload []byte) error {
	if len(payload) > maxPacketSize {
		return fmt.Errorf("payload too large: %d bytes", len(payload))
	}
	var header [tcpHeaderLength]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	return nil
}

func readFrame(r io.Reader) ([]byte, error) {
	var header [tcpHeaderLength]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size > maxPacketSize {
		return nil, fmt.Errorf("frame too large: %d bytes", size)
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func udpAddrsEqual(a, b *net.UDPAddr) bool {
	if a == nil || b == nil {
		return a == b
	}
	if !a.IP.Equal(b.IP) {
		return false
	}
	if a.Port != b.Port {
		return false
	}
	if len(a.Zone) > 0 || len(b.Zone) > 0 {
		return a.Zone == b.Zone
	}
	return true
}
