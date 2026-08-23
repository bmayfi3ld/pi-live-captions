// Package mdns advertises this host under a fixed name.local address for as
// long as the process runs, by shelling out to avahi-publish and holding the
// child process open. avahi-publish is a long-running foreground process
// that keeps the A record registered via D-Bus until it's killed, so tying
// its lifetime to ours withdraws the record automatically on shutdown.
package mdns

import (
	"log/slog"
	"net"
	"os/exec"
	"sync"
	"syscall"
)

// Publisher advertises <name>.local -> this host's LAN address for as long
// as the wrapped avahi-publish process runs.
type Publisher struct {
	cmd      *exec.Cmd
	stopped  chan struct{}
	stopOnce sync.Once
}

// Start spawns `avahi-publish -a -R <name>.local <ip>` in the background.
// Avahi is optional infrastructure, not a hard dependency: if the binary is
// missing, avahi-daemon isn't running, or the LAN address can't be
// determined, this logs a warning and returns nil rather than failing
// server startup.
func Start(name string, log *slog.Logger) *Publisher {
	if name == "" {
		return nil
	}

	ip, err := outboundIP()
	if err != nil {
		log.Warn("mdns: could not determine LAN address, skipping", "err", err)
		return nil
	}

	host := name + ".local"
	cmd := exec.Command("avahi-publish", "-a", "-R", host, ip.String())
	// Setpgid: a bare exec.Command child shares the parent's process group,
	// so a terminal Ctrl-C (SIGINT) hits it directly and it exits on its own
	// -- before Stop() ever runs and closes p.stopped -- which used to fire
	// a spurious "exited unexpectedly" warning on every ordinary shutdown.
	// Detaching it into its own group means only our own Stop() controls it.
	//
	// Pdeathsig: if this process dies without ever reaching Stop() (the
	// forced-exit os.Exit escape hatch in cmd/livecaption, or an unrecovered
	// panic -- both skip deferred cleanup entirely), the kernel still kills
	// the child instead of leaving it running forever advertising a
	// possibly-stale record.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGTERM,
	}
	if err := cmd.Start(); err != nil {
		log.Warn("mdns: avahi-publish unavailable, skipping (install avahi-utils to enable)", "err", err)
		return nil
	}

	p := &Publisher{cmd: cmd, stopped: make(chan struct{})}
	log.Info("mdns: advertising", "name", host, "addr", ip.String())

	go func() {
		err := cmd.Wait()
		select {
		case <-p.stopped:
			// Stop() already withdrew the record; this exit is expected.
		default:
			log.Warn("mdns: avahi-publish exited unexpectedly, is avahi-daemon running?", "err", err)
		}
	}()

	return p
}

// Stop withdraws the record (SIGTERM — matches avahi-publish's own observed
// graceful-exit handling: it prints "Got SIGTERM, quitting." and exits) and
// lets the background goroutine from Start reap the process. Safe to call on
// a nil *Publisher, and safe to call more than once.
func (p *Publisher) Stop() {
	if p == nil {
		return
	}
	p.stopOnce.Do(func() {
		close(p.stopped)
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Signal(syscall.SIGTERM)
		}
	})
}

// outboundIP asks the kernel which local address it would route through for
// a LAN/internet destination, without actually sending any packets (UDP
// dial doesn't touch the wire) or needing root.
func outboundIP() (net.IP, error) {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP, nil
}
