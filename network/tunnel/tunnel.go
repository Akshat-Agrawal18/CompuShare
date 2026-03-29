package tunnel

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/compushare/compushare/network/libp2p"
	"github.com/golang/glog"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
)

const TunnelProtocolID protocol.ID = "/compushare/tunnel/1.0"

// RegisterTunnelHandler registers a stream handler on the host that proxies
// incoming libp2p streams down to the specified local TCP port.
func RegisterTunnelHandler(host *libp2p.Host, localPort int) {
	host.Underlying().SetStreamHandler(TunnelProtocolID, func(s network.Stream) {
		glog.Infof("New incoming tunnel request from %s", s.Conn().RemotePeer())

		// Connect to the local sidecar TCP port
		localAddr := fmt.Sprintf("127.0.0.1:%d", localPort)
		conn, err := net.DialTimeout("tcp", localAddr, 5*time.Second)
		if err != nil {
			glog.Errorf("Failed to connect to local sidecar at %s: %v", localAddr, err)
			s.Reset()
			return
		}

		// Proxy data between libp2p stream and local TCP connection
		go proxyStream(s, conn)
		go proxyStream(conn, s)
	})
	glog.Infof("Tunnel handler registered for protocol %s -> local port %d", TunnelProtocolID, localPort)
}

// OpenTunnel establishes a libp2p stream to a remote peer running a tunnel handler
// and exposes it as a local net.Conn. Wait, for llamas.cpp RPC logic, we need to bind 
// to a LOCAL port on the consumer side so llama-cli can connect to it.
// OpenTunnel creates a local TCP listener and proxies all incoming connections 
// over new libp2p streams to the target peer.
func OpenTunnel(ctx context.Context, host *libp2p.Host, targetPeerID string) (int, error) {
	// Parse peer ID string
	pid, err := peer.Decode(targetPeerID)
	if err != nil {
		return 0, fmt.Errorf("invalid peer id: %w", err)
	}

	// 1. Start a local listener on a random port
	addr, err := net.ResolveTCPAddr("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return 0, err
	}

	localPort := l.Addr().(*net.TCPAddr).Port
	glog.Infof("Started local tunnel listener on port %d for remote peer %s", localPort, pid)

	// 2. Accept connections and proxy them
	go func() {
		defer l.Close()
		for {
			select {
			case <-ctx.Done():
				return // Shut down tunnel on context cancel
			default:
			}

			// Ensure we don't block forever if ctx is cancelled
			l.SetDeadline(time.Now().Add(1 * time.Second))
			conn, err := l.Accept()
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				if ctx.Err() != nil {
					return
				}
				glog.Errorf("Tunnel listener error: %v", err)
				continue
			}

			// Got a local connection (e.g. from llama-cli), open a new stream to the peer
			stream, err := host.Underlying().NewStream(ctx, pid, TunnelProtocolID)
			if err != nil {
				glog.Errorf("Failed to open libp2p stream to %s: %v", pid, err)
				conn.Close()
				continue
			}

			glog.Infof("Bridging local connection to peer %s", pid)
			go proxyStream(conn, stream)
			go proxyStream(stream, conn)
		}
	}()

	return localPort, nil
}

// proxyStream bidirectionally copies data between rw1 and rw2.
// It closes both ends when io.Copy finishes (i.e. one side disconnects).
func proxyStream(src io.ReadCloser, dst io.WriteCloser) {
	io.Copy(dst, src)
	src.Close()
	dst.Close()
}
