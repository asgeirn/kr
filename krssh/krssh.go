package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kryptco/kr"
	"github.com/kryptco/kr/krd"

	"github.com/keybase/saltpack/encoding/basex"
	"github.com/op/go-logging"
	"golang.org/x/crypto/ssh"
)

func fatal(msg string) {
	os.Stderr.WriteString(msg + "\r\n")
	os.Exit(1)
}

func useSyslog() bool {
	env := os.Getenv("KR_LOG_SYSLOG")
	if env != "" {
		return env == "true"
	}
	return true
}

var logger *logging.Logger = kr.SetupLogging("krssh", logging.INFO, useSyslog())

//	from https://github.com/golang/crypto/blob/master/ssh/messages.go#L98-L102
type kexECDHReplyMsg struct {
	HostKey         []byte `sshtype:"31|33"` //	handle SSH2_MSG_KEX_DH_GEX_REPLY as well
	EphemeralPubKey []byte
	Signature       []byte
}

func sendHostAuth(hostAuth kr.HostAuth) {
	conn, err := kr.HostAuthDial()
	if err != nil {
		os.Stderr.WriteString(kr.Red("Kryptonite ▶ Could not connect to Kryptonite daemon. Make sure it is running by typing \"kr restart\".\r\n"))
		return
	}
	defer conn.Close()
	json.NewEncoder(conn).Encode(hostAuth)
}

func tryParse(hostname string, onHostPrefix chan string, buf []byte) (err error) {
	kexECDHReplyTemplate := kexECDHReplyMsg{}
	err = ssh.Unmarshal(buf, &kexECDHReplyTemplate)
	if err == nil {
		hostAuth := kr.HostAuth{
			HostKey:   kexECDHReplyTemplate.HostKey,
			Signature: kexECDHReplyTemplate.Signature,
			HostNames: []string{hostname},
		}
		sigHash := sha256.Sum256(hostAuth.Signature)
		select {
		case onHostPrefix <- "[" + basex.Base62StdEncoding.EncodeToString(sigHash[:]) + "]":
		default:
		}
		sendHostAuth(hostAuth)
	}
	return
}

func parseSSHPacket(b []byte) (packet []byte) {
	if len(b) <= 4 {
		return
	}
	packetLen := binary.BigEndian.Uint32(b[:4])
	paddingLen := b[4]
	payloadLen := packetLen - uint32(paddingLen) - 1
	if payloadLen > (1<<18) || payloadLen < 1 || len(b) <= int(5+payloadLen) {
		return
	}
	packet = make([]byte, payloadLen)
	copy(packet, b[5:5+payloadLen])
	return
}

func startLogger(prefix string) (r kr.NotificationReader, err error) {
	r, err = kr.OpenNotificationReader(prefix)
	if err != nil {
		return
	}
	go func() {
		if prefix != "" {
			defer os.Remove(r.Name())
		}

		go func() {
			if krd.CheckIfUpdateAvailable(logger) {
				os.Stderr.WriteString(kr.Yellow("Kryptonite ▶ A new version of Kryptonite is available. Run \"kr upgrade\" to install it.\r\n"))
			}
		}()

		printedNotifications := map[string]bool{}
		for {
			notification, err := r.Read()
			switch err {
			case nil:
				notificationStr := string(notification)
				if _, ok := printedNotifications[notificationStr]; ok {
					continue
				}
				if strings.HasPrefix(notificationStr, "[") {
					if prefix != "" && strings.HasPrefix(notificationStr, prefix) {
						trimmed := strings.TrimPrefix(notificationStr, prefix)
						if strings.HasPrefix(trimmed, "STOP") {
							return
						}
						os.Stderr.WriteString(trimmed)
					}
				} else {
					if strings.Contains(notificationStr, "]") {
						//	skip malformed notification
						continue
					}
					os.Stderr.WriteString(notificationStr)
				}
				printedNotifications[notificationStr] = true
			case io.EOF:
				<-time.After(50 * time.Millisecond)
			default:
				return
			}
		}
	}()
	return
}

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		fatal("not enough arguments")
	}
	var host, port string
	host = os.Args[1]
	if len(os.Args) >= 3 {
		port = os.Args[2]
	} else {
		port = "22"
	}

	notifyPrefix := make(chan string, 1)
	startLogger("")
	go func() {
		prefix := <-notifyPrefix
		startLogger(prefix)
	}()

	remoteConn, err := net.Dial("tcp", host+":"+port)
	if err != nil {
		fatal(kr.Red("could not connect to remote: " + err.Error()))
	}

	remoteDoneChan := make(chan bool)

	go func() {
		func() {
			buf := make([]byte, 1<<18)
			packetNum := 0
			for {
				n, err := remoteConn.Read(buf)
				if err != nil {
					return
				}
				if n > 0 {
					packetNum++
					if packetNum > 1 {
						sshPacket := parseSSHPacket(buf)
						tryParse(host, notifyPrefix, sshPacket)
					}
					byteBuf := bytes.NewBuffer(buf[:n])
					_, err := byteBuf.WriteTo(os.Stdout)
					if err != nil {
						return
					}
				}
			}
		}()
		remoteDoneChan <- true
	}()

	localDoneChan := make(chan bool)

	go func() {
		func() {
			buf := make([]byte, 1<<18)
			for {
				n, err := os.Stdin.Read(buf)
				if err != nil {
					return
				}
				if n > 0 {
					byteBuf := bytes.NewBuffer(buf[:n])
					_, err := byteBuf.WriteTo(remoteConn)
					if err != nil {
						return
					}
				}
			}
		}()
		localDoneChan <- true
	}()

	stopSignal := make(chan os.Signal, 1)
	signal.Notify(stopSignal, os.Interrupt, os.Kill, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTERM)
	for {
		select {
		case <-stopSignal:
			return
		case <-localDoneChan:
			os.Stdout.Sync()
			<-time.After(500 * time.Millisecond)
			return
		case <-remoteDoneChan:
			os.Stdout.Sync()
			<-time.After(500 * time.Millisecond)
			return
		}
	}
}
