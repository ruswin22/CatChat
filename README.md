# CatChat — Retro Encrypted LAN Chat

## Quick Start
Pre-requsites: Golang (go1.25.3)

### For Windows
```bash
go mod tidy
go build -o catchat-server.exe ./server
go build -o catchat-client.exe ./client
./catchat-server   # on your machine
./catchat-client   # on any LAN machine
```

### For Linux
```bash
go mod tidy
go build -o catchat-server ./server
go build -o catchat-client ./client
./catchat-server   # on your machine
./catchat-client   # on any LAN machine
```

## Linux Firewall
```bash
sudo ufw allow 4242/tcp
```

## Structure
```
catchat/
├── go.mod
├── shared/shared.go    # protocol, ring buffer, TLS, splash
├── server/main.go      # hub, connections, listener
├── server/tui.go       # chat view + Tab admin dashboard
└── client/main.go      # client TUI + handshake
```

## Commands (in chat)
- /list      show connected users
- /history   load full message history
- Ctrl+C     quit

## Admin Dashboard (server only, press Tab)
- up/down    navigate users
- k + Enter  kick selected
- b + Enter  ban selected
- Enter on banned IP = unban
