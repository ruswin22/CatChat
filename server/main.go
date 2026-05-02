package main

import (
	"bufio"
	"catchat/shared"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ── Client ────────────────────────────────────────────────────────────────────

type Client struct {
	conn     net.Conn
	name     string
	colour   string
	ip       string
	joinedAt time.Time
	lastSeen time.Time
	send     chan string
	typing   bool

	// closed is set to true under Hub.mu when the client is unregistered.
	// safeSend checks this before writing to avoid panics on a closed channel.
	closed bool
}

func (c *Client) Duration() string {
	d := time.Since(c.joinedAt).Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

// ── Identity store ────────────────────────────────────────────────────────────

type Identity struct {
	Name   string
	Colour string
}

// ── Hub ───────────────────────────────────────────────────────────────────────

type Hub struct {
	mu         sync.RWMutex
	clients    map[*Client]struct{}
	identities map[string]Identity // ip → identity
	blacklist  map[string]struct{} // ip → banned
	history    *shared.RingBuffer

	serverClient *Client

	serverInbox chan string
	dashRefresh chan struct{}
}

func newHub(serverInbox chan string, dashRefresh chan struct{}) *Hub {
	return &Hub{
		clients:     make(map[*Client]struct{}),
		identities:  make(map[string]Identity),
		blacklist:   make(map[string]struct{}),
		history:     shared.NewRingBuffer(shared.HistorySize),
		serverInbox: serverInbox,
		dashRefresh: dashRefresh,
	}
}

func (h *Hub) isNameTaken(name string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	nl := strings.ToLower(name)
	if h.serverClient != nil && strings.ToLower(h.serverClient.name) == nl {
		return true
	}
	for c := range h.clients {
		if strings.ToLower(c.name) == nl {
			return true
		}
	}
	return false
}

func (h *Hub) register(c *Client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	select {
	case h.dashRefresh <- struct{}{}:
	default:
	}
}

func (h *Hub) unregister(c *Client) {
	h.mu.Lock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		c.closed = true
		close(c.send)
	}
	h.mu.Unlock()
	select {
	case h.dashRefresh <- struct{}{}:
	default:
	}
}

// safeSend enqueues a line without blocking; drops if buffer is full or
// the channel is already closed. Safe to call while holding hub locks.
func safeSend(c *Client, line string) {
	defer func() { recover() }()
	select {
	case c.send <- line:
	default: // drop — slow/dead client
	}
}

// safeSendBlock enqueues a line and waits until space is available.
// It recovers from panics caused by sending on a closed channel.
// Do NOT call while holding hub.mu — only use for targeted sends (Kick/Ban).
func safeSendBlock(c *Client, line string) {
	defer func() { recover() }()
	c.send <- line
}

func (h *Hub) broadcast(line string, except *Client) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	select {
	case h.serverInbox <- line:
	default:
	}
	for c := range h.clients {
		if c == except {
			continue
		}
		safeSend(c, line)
	}
}

func (h *Hub) addHistory(m shared.Message) {
	h.mu.Lock()
	h.history.Push(m)
	h.mu.Unlock()
}

func (h *Hub) sendHistory(c *Client, all bool) {
	h.mu.RLock()
	var msgs []shared.Message
	if all {
		msgs = h.history.All()
	} else {
		msgs = h.history.Last(shared.RecentCount)
	}
	h.mu.RUnlock()

	if len(msgs) == 0 {
		safeSend(c, shared.Encode(shared.TagCtl, "HST_EMPTY"))
		return
	}
	safeSend(c, shared.Encode(shared.TagHistStart, fmt.Sprintf("%d", len(msgs))))
	for _, m := range msgs {
		safeSend(c, shared.Encode(shared.TagMsg, m.Wire()))
	}
	safeSend(c, shared.Encode(shared.TagHistEnd, ""))
}

func (h *Hub) connectedList() []*Client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*Client, 0, len(h.clients))
	for c := range h.clients {
		out = append(out, c)
	}
	return out
}

func (h *Hub) BlacklistedIPs() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]string, 0, len(h.blacklist))
	for ip := range h.blacklist {
		out = append(out, ip)
	}
	return out
}

func (h *Hub) Unblacklist(ip string) {
	h.mu.Lock()
	delete(h.blacklist, ip)
	h.mu.Unlock()
}

// Kick sends a KICKED notice to the client then closes its connection.
func (h *Hub) Kick(c *Client) {
	safeSendBlock(c, shared.Encode(shared.TagCtl, "KICKED"))
	// Give the write goroutine a brief window to flush before teardown.
	time.Sleep(100 * time.Millisecond)
	c.conn.Close()

	msg := fmt.Sprintf("%s was kicked by the server", c.name)
	sysMsg := shared.Message{Timestamp: time.Now(), IsSystem: true, Text: msg}
	h.addHistory(sysMsg)
	h.broadcast(shared.Encode(shared.TagSys, sysMsg.Wire()), c)
}

// Ban blacklists the client's IP, sends a BANNED notice, then disconnects.
func (h *Hub) Ban(c *Client) {
	h.mu.Lock()
	h.blacklist[c.ip] = struct{}{}
	h.mu.Unlock()

	safeSendBlock(c, shared.Encode(shared.TagCtl, "BANNED"))
	time.Sleep(100 * time.Millisecond)
	c.conn.Close()

	msg := fmt.Sprintf("%s was banned by the server", c.name)
	sysMsg := shared.Message{Timestamp: time.Now(), IsSystem: true, Text: msg}
	h.addHistory(sysMsg)
	h.broadcast(shared.Encode(shared.TagSys, sysMsg.Wire()), c)
}

// ── Handle one client connection ──────────────────────────────────────────────

func (h *Hub) handle(conn net.Conn) {
	defer conn.Close()

	rawIP, _, _ := net.SplitHostPort(conn.RemoteAddr().String())

	// Check blacklist.
	h.mu.RLock()
	_, banned := h.blacklist[rawIP]
	h.mu.RUnlock()
	if banned {
		fmt.Fprint(conn, shared.Encode(shared.TagCtl, "BANNED"))
		return
	}

	// Check capacity.
	h.mu.RLock()
	count := len(h.clients)
	h.mu.RUnlock()
	if count >= shared.MaxClients {
		fmt.Fprint(conn, shared.Encode(shared.TagCtl, "FULL"))
		return
	}

	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)

	sendLine := func(tag, payload string) {
		fmt.Fprintf(writer, "%s", shared.Encode(tag, payload))
		writer.Flush()
	}

	readLine := func() (string, error) {
		line, err := reader.ReadString('\n')
		return strings.TrimRight(line, "\r\n"), err
	}

	// ── Identity handshake ────────────────────────────────────────────────────
	h.mu.RLock()
	ident, known := h.identities[rawIP]
	h.mu.RUnlock()

	var name, colour string

	if known {
		sendLine(shared.TagWho, "RETURNING|"+ident.Name+"|"+ident.Colour)
		name = ident.Name
		colour = ident.Colour
		if h.isNameTaken(name) {
			known = false
		}
	}

	if !known {
		sendLine(shared.TagWho, "NAME")
		for {
			raw, err := readLine()
			if err != nil {
				return
			}
			tag, payload, ok := shared.Decode(raw)
			if !ok || tag != shared.TagWho {
				continue
			}
			n := strings.TrimSpace(payload)
			if n == "" {
				sendLine(shared.TagWho, "NAME_EMPTY")
				continue
			}
			if len([]rune(n)) > 9 {
				sendLine(shared.TagWho, "NAME_LONG")
				continue
			}
			if strings.Contains(n, "|") {
				sendLine(shared.TagWho, "NAME_INVALID")
				continue
			}
			if h.isNameTaken(n) {
				sendLine(shared.TagWho, "NAME_TAKEN")
				continue
			}
			name = n
			break
		}

		sendLine(shared.TagWho, "COLOUR")
		for {
			raw, err := readLine()
			if err != nil {
				return
			}
			tag, payload, ok := shared.Decode(raw)
			if !ok || tag != shared.TagWho {
				continue
			}
			c := strings.TrimSpace(payload)
			idx := -1
			for i, col := range shared.Colours {
				if c == fmt.Sprintf("%d", i+1) || strings.EqualFold(c, col.Name) {
					idx = i
					break
				}
			}
			if idx < 0 {
				sendLine(shared.TagWho, "COLOUR_INVALID")
				continue
			}
			colour = shared.Colours[idx].Code
			break
		}

		h.mu.Lock()
		h.identities[rawIP] = Identity{Name: name, Colour: colour}
		h.mu.Unlock()
	}

	sendLine(shared.TagAck, "OK|"+name+"|"+colour)

	c := &Client{
		conn:     conn,
		name:     name,
		colour:   colour,
		ip:       rawIP,
		joinedAt: time.Now(),
		lastSeen: time.Now(),
		send:     make(chan string, 128),
	}

	h.register(c)
	defer h.unregister(c)

	// Write goroutine — drains c.send until the channel is closed.
	go func() {
		w := bufio.NewWriter(conn)
		for line := range c.send {
			fmt.Fprint(w, line)
			// Flush after each message if nothing else is buffered.
			if len(c.send) == 0 {
				w.Flush()
			}
		}
		w.Flush()
	}()

	// Send recent history on join.
	h.sendHistory(c, false)

	// Announce join.
	joinMsg := shared.Message{
		Timestamp: time.Now(),
		IsSystem:  true,
		Text:      fmt.Sprintf("%s has joined the chat", name),
	}
	h.addHistory(joinMsg)
	h.broadcast(shared.Encode(shared.TagSys, joinMsg.Wire()), nil)

	// Idle timer.
	idleTimer := time.NewTimer(shared.IdleTimeout)
	defer idleTimer.Stop()

	// Async scanner so we can also select on the idle timer.
	scanCh := make(chan string, 8)
	errCh := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(reader)
		for sc.Scan() {
			scanCh <- sc.Text()
		}
		errCh <- sc.Err()
	}()

	disconnectMsg := fmt.Sprintf("%s has left the chat", name)

	for {
		select {
		case <-idleTimer.C:
			disconnectMsg = fmt.Sprintf("%s was disconnected due to inactivity", name)
			goto disconnect

		case err := <-errCh:
			_ = err
			goto disconnect

		case raw := <-scanCh:
			c.lastSeen = time.Now()
			idleTimer.Reset(shared.IdleTimeout)

			tag, payload, ok := shared.Decode(raw)
			if !ok {
				continue
			}

			switch tag {
			case shared.TagQry:
				switch payload {
				case "/list":
					clients := h.connectedList()
					// Send one TagCmd per line — the scanner splits on '\n' so a
					// single message with embedded newlines would lose all lines
					// after the first on the receiving end.
					safeSend(c, shared.Encode(shared.TagCmd,
						fmt.Sprintf(">> Connected users (%d):", len(clients)+1)))
					safeSend(c, shared.Encode(shared.TagCmd,
						fmt.Sprintf("   %s%s%s",
							h.serverClient.colour, h.serverClient.name, shared.Reset)))
					for _, cl := range clients {
						safeSend(c, shared.Encode(shared.TagCmd,
							fmt.Sprintf("   %s%s%s",
								cl.colour, cl.name, shared.Reset)))
					}

				case "/history":
					h.sendHistory(c, true)
				}

			case shared.TagTyp:
				h.broadcast(shared.Encode(shared.TagTyp, name), c)

			case shared.TagClr:
				h.broadcast(shared.Encode(shared.TagClr, name), c)

			case shared.TagMsg:
				text := payload
				if len([]rune(text)) > shared.MaxMessageLen {
					safeSend(c, shared.Encode(shared.TagCtl, "MSG_TOO_LONG"))
					continue
				}
				m := shared.Message{
					Timestamp: time.Now(),
					Sender:    name,
					Colour:    colour,
					Text:      text,
				}
				h.addHistory(m)
				h.broadcast(shared.Encode(shared.TagMsg, m.Wire()), nil)
			}
		}
	}

disconnect:
	leaveMsg := shared.Message{Timestamp: time.Now(), IsSystem: true, Text: disconnectMsg}
	h.addHistory(leaveMsg)
	h.broadcast(shared.Encode(shared.TagSys, leaveMsg.Wire()), c)
}

// ── Server TUI entry point ────────────────────────────────────────────────────

func serverTUI(hub *Hub, port, localIP string) {
	tui := newServerTUI(hub, port, localIP)
	tui.run()
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	fmt.Print("\033[2J\033[H")
	fmt.Println(shared.Bold + shared.Colours[4].Code + shared.Splash + shared.Reset)
	fmt.Println(shared.SysColour + "  ══════════════════════════════════════════════════" + shared.Reset)
	fmt.Println("  SERVER SETUP")
	fmt.Println(shared.SysColour + "  ══════════════════════════════════════════════════" + shared.Reset)
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)
	prompt := func(q string) string {
		fmt.Print("  >> " + q + ": ")
		line, _ := reader.ReadString('\n')
		return strings.TrimRight(line, "\r\n")
	}

	// Name
	var name string
	for {
		n := strings.TrimSpace(prompt("Your display name"))
		if n == "" {
			fmt.Println("  Name cannot be empty.")
			continue
		}
		if len([]rune(n)) > 20 {
			fmt.Println("  Name too long (max 20 chars).")
			continue
		}
		if strings.Contains(n, "|") {
			fmt.Println("  Name cannot contain '|' character.")
			continue
		}
		name = n
		break
	}

	// Colour
	fmt.Println()
	fmt.Println("  Choose a colour:")
	for i, c := range shared.Colours {
		fmt.Printf("    %d. %s%s%s\n", i+1, c.Code, c.Name, shared.Reset)
	}
	var colour string
	for {
		c := strings.TrimSpace(prompt("Colour (1-6)"))
		idx := -1
		for i, col := range shared.Colours {
			if c == fmt.Sprintf("%d", i+1) || strings.EqualFold(c, col.Name) {
				idx = i
				break
			}
		}
		if idx < 0 {
			fmt.Println("  Invalid choice.")
			continue
		}
		colour = shared.Colours[idx].Code
		break
	}

	// Port
	fmt.Println()
	portRaw := strings.TrimSpace(prompt(fmt.Sprintf("Port (press enter for %s)", shared.DefaultPort)))
	if portRaw == "" {
		portRaw = shared.DefaultPort
	}

	// Detect local IP before binding so we can show it in the TUI.
	localIP := shared.GetLocalIP()

	// TLS
	tlsCfg, err := shared.GenerateTLSConfig()
	if err != nil {
		fmt.Println("  Failed to generate TLS config:", err)
		os.Exit(1)
	}

	addr := ":" + portRaw
	ln, err := tls.Listen("tcp", addr, tlsCfg)
	if err != nil {
		fmt.Println("  Failed to listen:", err)
		os.Exit(1)
	}

	fmt.Println()
	fmt.Printf("  %s>> CatChat server started on %s:%s%s\n",
		shared.SysColour, localIP, portRaw, shared.Reset)
	if runtime.GOOS == "linux" {
		fmt.Printf("  %s>> Linux detected: if clients can't connect, run: sudo ufw allow %s/tcp%s\n",
			shared.SysColour, portRaw, shared.Reset)
	}
	fmt.Println()
	time.Sleep(1200 * time.Millisecond)

	serverInbox := make(chan string, 256)
	dashRefresh := make(chan struct{}, 4)

	hub := newHub(serverInbox, dashRefresh)
	hub.serverClient = &Client{
		name:     name,
		colour:   colour,
		joinedAt: time.Now(),
		lastSeen: time.Now(),
		send:     make(chan string, 128),
	}

	// Accept loop
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go hub.handle(conn)
		}
	}()

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		shutdownMsg := shared.Message{
			Timestamp: time.Now(),
			IsSystem:  true,
			Text:      "CatChat server is shutting down. Goodbye!",
		}
		hub.broadcast(shared.Encode(shared.TagSys, shutdownMsg.Wire()), nil)
		time.Sleep(500 * time.Millisecond)
		ln.Close()
		os.Exit(0)
	}()

	serverTUI(hub, portRaw, localIP)
}