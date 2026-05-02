package shared

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"
)

// ── Colours ───────────────────────────────────────────────────────────────────

type Colour struct {
	Name string
	Code string // ANSI escape
}

var Colours = []Colour{
	{Name: "red", Code: "\033[31m"},
	{Name: "green", Code: "\033[32m"},
	{Name: "yellow", Code: "\033[33m"},
	{Name: "blue", Code: "\033[34m"},
	{Name: "magenta", Code: "\033[35m"},
	{Name: "cyan", Code: "\033[36m"},
}

const Reset = "\033[0m"
const Bold = "\033[1m"
const Dim = "\033[2m"
const Blink = "\033[5m"

// Colour codes for system messages
const SysColour = "\033[33m"   // yellow for system
const AdminColour = "\033[35m" // magenta for admin actions

// ── Protocol ──────────────────────────────────────────────────────────────────
// Each line sent over the wire: 3-char tag + '|' + payload + '\n'

const (
	TagMsg      = "MSG"
	TagSys      = "SYS"
	TagTyp      = "TYP"
	TagClr      = "CLR"
	TagCmd      = "CMD"
	TagQry      = "QRY"
	TagCtl      = "CTL"
	TagWho      = "WHO"
	TagAck      = "ACK"
	TagHistStart = "HST"
	TagHistEnd   = "HEN"
)

func Encode(tag, payload string) string {
	return tag + "|" + payload + "\n"
}

func Decode(line string) (tag, payload string, ok bool) {
	line = strings.TrimRight(line, "\r\n")
	if len(line) < 4 || line[3] != '|' {
		return "", "", false
	}
	return line[:3], line[4:], true
}

// ── Network helpers ───────────────────────────────────────────────────────────

// GetLocalIP returns the preferred outbound local IP of this machine.
// It dials a UDP address (no packet is actually sent) to discover which
// interface the OS would route traffic through, then returns that IP.
// Falls back to "127.0.0.1" on any error.
func GetLocalIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

// ── Ring buffer ───────────────────────────────────────────────────────────────

type Message struct {
	Timestamp time.Time
	Sender    string
	Colour    string
	Text      string
	IsSystem  bool
}

func (m Message) Format() string {
	ts := m.Timestamp.Format("15:04:05")
	if m.IsSystem {
		return fmt.Sprintf("%s[%s] >> %s%s", SysColour, ts, m.Text, Reset)
	}
	return fmt.Sprintf("%s[%s]%s %s%s%s: %s", Dim, ts, Reset, m.Colour, m.Sender, Reset, m.Text)
}

// Wire format: "timestamp|sender|colour|issystem|text"
func (m Message) Wire() string {
	sys := "0"
	if m.IsSystem {
		sys = "1"
	}
	return fmt.Sprintf("%s|%s|%s|%s|%s",
		m.Timestamp.Format(time.RFC3339),
		m.Sender,
		m.Colour,
		sys,
		m.Text,
	)
}

func ParseWireMessage(s string) (Message, bool) {
	parts := strings.SplitN(s, "|", 5)
	if len(parts) != 5 {
		return Message{}, false
	}
	t, err := time.Parse(time.RFC3339, parts[0])
	if err != nil {
		return Message{}, false
	}
	return Message{
		Timestamp: t,
		Sender:    parts[1],
		Colour:    parts[2],
		IsSystem:  parts[3] == "1",
		Text:      parts[4],
	}, true
}

type RingBuffer struct {
	buf   []Message
	size  int
	head  int
	count int
}

func NewRingBuffer(size int) *RingBuffer {
	return &RingBuffer{buf: make([]Message, size), size: size}
}

func (r *RingBuffer) Push(m Message) {
	r.buf[r.head] = m
	r.head = (r.head + 1) % r.size
	if r.count < r.size {
		r.count++
	}
}

// All returns messages oldest→newest.
func (r *RingBuffer) All() []Message {
	if r.count == 0 {
		return nil
	}
	out := make([]Message, r.count)
	start := (r.head - r.count + r.size) % r.size
	for i := 0; i < r.count; i++ {
		out[i] = r.buf[(start+i)%r.size]
	}
	return out
}

// Last returns up to n most-recent messages.
func (r *RingBuffer) Last(n int) []Message {
	all := r.All()
	if len(all) <= n {
		return all
	}
	return all[len(all)-n:]
}

// ── TLS ───────────────────────────────────────────────────────────────────────

func GenerateTLSConfig() (*tls.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{Organization: []string{"CatChat"}},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(24 * time.Hour * 365),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

func ClientTLSConfig() *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true, // self-signed cert on LAN; traffic is still encrypted
		MinVersion:         tls.VersionTLS12,
	}
}

// ── Splash screen ─────────────────────────────────────────────────────────────

const Splash = `
⠀⡿⢘⠀⠀⢠⠆⠀⠀⠀⡰⠬⢉⣽⣿⣿⡿⠟⠋⠁⠀⢿⣞⣷⣾⣿⣿⣿     
⢀⡟⡌⠀⠀⢐⠌⠀⠀⠄⡡⠃⠞⠛⠉⠀⠀⠀⠀⠀⠀⣾⣿⣿⣿⣿⣿⣿   	  
⣤⡇⡆⠁⠀⢘⡎⢤⠀⠀⠁⠀⠀⠀⠀⠀⠂⠠⠄⠂⢀⢹⣿⣿⣿⢸⣿⣿⣿    
⢸⡇⠅⠂⠀⡸⡜⢢⠅⠀⣀⡀⡀⠤⢶⣺⣿⡒⢄⠀⢭⣘⡻⣿⣿⢸⣿⣿⣿   
⢾⡀⠁⠆⠀⢱⡎⢁⣶⣿⣿⠁⣸⡇⢸⣇⣿⣷⢸⣆⠀⣿⡿⣆⠹⢾⣿⣿⣿	    ██████╗  █████╗ ████████╗ ██████╗ ██╗  ██╗  █████╗ ████████╗
⢺⢸⠀⠀⠀⢎⣴⣾⠿⠟⠿⣷⣿⣫⣾⣿⣿⣿⡎⣿⠿⠟⠉⠚⠻⣮⡙⣿	   ██╔════╝ ██╔══██╗╚══██╔══╝██╔════╝ ██║  ██║ ██╔══██╗╚══██╔══╝
⣻⢨⠐⠀⣠⣾⠿⠉⠀⠀⠀⠀⠙⢿⣿⣿⣿⣿⣷⣅⠀⠀⠀⠀⠀⢩⣙⠚       ██║      ███████║   ██║   ██║      ███████║ ███████║   ██║
⣽⠰⠀⠈⠁⣠⣶⡀⠀⠀⠀⢀⢀⣾⣿⣿⣿⡿⣿⣿⣤⣤⣀⣀⣤⣾⠿⠇	   ██║      ██╔══██║   ██║   ██║      ██╔══██║ ██╔══██║   ██║
⣿⠆⠀⠀⡀⠙⠉⢀⢽⣿⣿⣿⣿⡿⢏⠿⡿⡭⢭⠻⣿⣿⣿⣿⣿⣷⣾⠐	   ╚██████╗ ██║  ██║   ██║   ╚██████╗ ██║  ██║ ██║  ██║   ██║
⢻⡃⠀⣀⣠⡄⠀⣈⣺⣿⢿⣿⣿⣿⣦⣀⠓⠃⣠⣾⣿⣛⣿⣟⣿⣿⠷⢿        ╚═════╝ ╚═╝  ╚═╝   ╚═╝    ╚═════╝ ╚═╝  ╚═╝ ╚═╝  ╚═╝   ╚═╝
⠀⠀⢐⣾⠗⠈⠁⠤⣾⣷⣿⣾⣿⣿⣿⣿⡅⢌⣿⣿⣿⣿⣷⣿⣿⣽⣏⡀         ≡≡  Retro LAN Chat  ·  Encrypted  ·  Terminal Native  ≡≡
⠀⠄⠀⣀⠄⡠⣴⣾⣿⣿⣿⣿⣿⣷⣶⣤⣴⣦⣬⣴⣿⣿⣿⣿⣿⣿⣿⣶		 
⠀⠂⠈⣡⣾⣾⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿
⠀⠀⢱⣿⣷⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿
`

const MaxMessageLen = 500
const MaxClients = 15
const HistorySize = 1000
const RecentCount = 50
const IdleTimeout = 30 * time.Minute
const DefaultPort = "4242"