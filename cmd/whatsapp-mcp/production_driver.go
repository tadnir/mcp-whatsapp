package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/sealjay/mcp-whatsapp/internal/client"
)

// pairRetryDelay is how long the pairing loop waits before trying again
// after a failed reconnect.
const pairRetryDelay = 10 * time.Second

// freshPairingWindow is how young a pairing connection must be for a
// phone-number code request to reuse it. whatsmeow ends every pairing
// connection when its QR codes run out (about 2m40s after it connects), and
// that also ends a phone-number code typed after that moment, so an older
// connection is replaced before the code is requested.
const freshPairingWindow = 10 * time.Second

// productionDriver adapts *client.Client to the daemon.pairDriver interface.
// It does not install any internal state — the Client itself owns all
// whatsmeow-facing state — apart from the pairing loop below.
type productionDriver struct {
	c *client.Client

	// Pairing loop state. Each pairing connection is a "cycle": a fresh
	// QR channel plus a Connect. whatsmeow closes the channel and
	// disconnects when the cycle's QR codes run out, so the loop starts a
	// new cycle until pairing succeeds, the context ends, or Stop.
	mu         sync.Mutex
	pairing    bool
	cycle      int
	cycleStart time.Time
	stop       chan struct{} // closed to end the current cycle's watcher
	ready      chan struct{} // closed at the current cycle's first QR code
	pairCtx    context.Context
	onQR       func(string)
	onSuccess  func()
}

func newProductionDriver(c *client.Client) *productionDriver {
	return &productionDriver{c: c}
}

func (p *productionDriver) IsLoggedIn() bool { return p.c.IsLoggedIn() }

func (p *productionDriver) StartPairing(ctx context.Context, onQR func(string), onSuccess func()) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pairing = true
	p.pairCtx = ctx
	p.onQR = onQR
	p.onSuccess = onSuccess
	return p.startCycleLocked()
}

// startCycleLocked opens a new QR channel, connects, and watches the channel
// in the background. Callers hold p.mu.
func (p *productionDriver) startCycleLocked() error {
	if p.stop != nil {
		close(p.stop)
		p.stop = nil
	}
	ctx := p.pairCtx
	qrCh, err := p.c.QRChannel(ctx)
	if err != nil {
		return err
	}
	p.cycle++
	cycle := p.cycle
	stop := make(chan struct{})
	ready := make(chan struct{})
	p.stop = stop
	p.ready = ready
	p.cycleStart = time.Now()
	go p.watchCycle(ctx, cycle, stop, ready, qrCh)
	// Must call Connect AFTER QRChannel per whatsmeow docs.
	return p.c.Connect(ctx, client.ConnectOpts{AllowUnpaired: true})
}

func (p *productionDriver) watchCycle(ctx context.Context, cycle int, stop <-chan struct{}, ready chan<- struct{}, qrCh <-chan whatsmeow.QRChannelItem) {
	readyClosed := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case item, ok := <-qrCh:
			if ok {
				switch item.Event {
				case "code":
					p.onQR(item.Code)
					if !readyClosed {
						// whatsmeow: a pairing code may be requested once the
						// first QR code has arrived.
						close(ready)
						readyClosed = true
					}
				case "success":
					p.mu.Lock()
					p.pairing = false
					p.mu.Unlock()
					p.onSuccess()
					return
				}
				continue
			}
			// The channel closed without a success: the QR codes ran out
			// (or the socket dropped) and whatsmeow disconnected. Start a
			// new cycle, so the pairing page keeps a live QR code and a
			// phone-number code can still be requested.
			p.restartAfter(ctx, cycle, 0)
			return
		}
	}
}

// restartAfter starts a new pairing cycle after delay, unless a newer cycle
// has started, pairing has finished, or ctx is done. On failure it tries
// again after pairRetryDelay.
func (p *productionDriver) restartAfter(ctx context.Context, cycle int, delay time.Duration) {
	for {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		}
		p.mu.Lock()
		if !p.pairing || p.cycle != cycle || ctx.Err() != nil || p.c.IsLoggedIn() {
			p.mu.Unlock()
			return
		}
		p.c.Disconnect()
		err := p.startCycleLocked()
		p.mu.Unlock()
		if err == nil {
			return
		}
		fmt.Fprintf(os.Stderr, "pairing: restarting after the QR codes ran out failed: %v\n", err)
		p.mu.Lock()
		cycle = p.cycle
		p.mu.Unlock()
		delay = pairRetryDelay
	}
}

func (p *productionDriver) Connect(ctx context.Context, onLoggedOut func()) error {
	p.c.AddLoggedOutHandler(func(_ *events.LoggedOut) { onLoggedOut() })
	p.c.StartEventHandler()
	if p.c.IsConnected() {
		// The pairing flow already connected this client; just install
		// the post-pair handlers and return.
		return nil
	}
	return p.c.Connect(ctx, client.ConnectOpts{})
}

func (p *productionDriver) Logout(ctx context.Context) error { return p.c.Logout(ctx) }

// PairPhone lets the daemon's /pair/code endpoint trigger the WhatsApp
// "link with phone number" flow on the underlying client. Unless the current
// pairing connection is only seconds old, it starts a fresh one first, so the
// code gets the whole life of a pairing connection to be typed in.
func (p *productionDriver) PairPhone(ctx context.Context, phone string) (string, error) {
	p.mu.Lock()
	if p.pairing && time.Since(p.cycleStart) > freshPairingWindow {
		p.c.Disconnect()
		if err := p.startCycleLocked(); err != nil {
			p.mu.Unlock()
			return "", fmt.Errorf("reconnect for pairing: %w", err)
		}
	}
	ready := p.ready
	p.mu.Unlock()
	if ready != nil {
		select {
		case <-ready:
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(15 * time.Second):
			return "", fmt.Errorf("pairing connection not ready yet; try again")
		}
	}
	return p.c.PairPhone(ctx, phone)
}

func (p *productionDriver) Disconnect() {
	p.mu.Lock()
	p.pairing = false
	if p.stop != nil {
		close(p.stop)
		p.stop = nil
	}
	p.mu.Unlock()
	p.c.Disconnect()
}
