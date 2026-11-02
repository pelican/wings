package sftp

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ed25519"
	"golang.org/x/crypto/ssh"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/plugins"
	"github.com/pelican/wings/plugins/api"
	"github.com/pelican/wings/remote"
	"github.com/pelican/wings/server"
)

// Usernames all follow the same format, so don't even bother hitting the API if the username is not
// at least in the expected format. This is very basic protection against random bots finding the SFTP
// server and sending a flood of usernames.
var validUsernameRegexp = regexp.MustCompile(`^(?i)(.+)\.([a-z0-9]{8})$`)

//goland:noinspection GoNameStartsWithPackageName
type SFTPServer struct {
	manager  *server.Manager
	BasePath string
	ReadOnly bool
	Listen   string
}

func New(m *server.Manager) *SFTPServer {
	cfg := config.Get().System
	return &SFTPServer{
		manager:  m,
		BasePath: cfg.Data,
		ReadOnly: cfg.Sftp.ReadOnly,
		Listen:   cfg.Sftp.Address + ":" + strconv.Itoa(cfg.Sftp.Port),
	}
}

// Run starts the SFTP server and add a persistent listener to handle inbound
// SFTP connections. This will automatically generate an ED25519 key if one does
// not already exist on the system for host key verification purposes.
func (c *SFTPServer) Run() error {
	if _, err := os.Stat(c.PrivateKeyPath()); os.IsNotExist(err) {
		if err := c.generateED25519PrivateKey(); err != nil {
			return err
		}
	} else if err != nil {
		return errors.Wrap(err, "sftp: could not stat private key file")
	}
	pb, err := os.ReadFile(c.PrivateKeyPath())
	if err != nil {
		return errors.Wrap(err, "sftp: could not read private key file")
	}
	private, err := ssh.ParsePrivateKey(pb)
	if err != nil {
		return err
	}

	conf := &ssh.ServerConfig{
		Config: ssh.Config{
			KeyExchanges: []string{
				"curve25519-sha256", "curve25519-sha256@libssh.org",
				"ecdh-sha2-nistp256", "ecdh-sha2-nistp384", "ecdh-sha2-nistp521",
				"diffie-hellman-group14-sha256",
			},
			Ciphers: []string{
				"aes128-gcm@openssh.com",
				"chacha20-poly1305@openssh.com",
				"aes128-ctr", "aes192-ctr", "aes256-ctr",
			},
			MACs: []string{
				"hmac-sha2-256-etm@openssh.com", "hmac-sha2-256",
			},
		},
		NoClientAuth: false,
		MaxAuthTries: 6,
		PasswordCallback: func(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			return c.makeCredentialsRequest(conn, remote.SftpAuthPassword, string(password))
		},
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			return c.makeCredentialsRequest(conn, remote.SftpAuthPublicKey, string(ssh.MarshalAuthorizedKey(key)))
		},
	}
	conf.AddHostKey(private)

	listener, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}

	public := string(ssh.MarshalAuthorizedKey(private.PublicKey()))
	log.WithField("listen", c.Listen).WithField("public_key", strings.Trim(public, "\n")).Info("sftp server listening for connections")

	for {
		if conn, _ := listener.Accept(); conn != nil {
			go func(conn net.Conn) {
				defer conn.Close()
				if err := c.AcceptInbound(conn, conf); err != nil {
					log.WithField("error", err).WithField("ip", conn.RemoteAddr().String()).Error("sftp: failed to accept inbound connection")
				}
			}(conn)
		}
	}
}

// AcceptInbound handles an inbound connection to the instance and determines if we should
// serve the request or not.
func (c *SFTPServer) AcceptInbound(conn net.Conn, config *ssh.ServerConfig) error {
	// Before beginning a handshake must be performed on the incoming net.Conn
	sconn, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return errors.WithStack(err)
	}
	defer sconn.Close()
	go ssh.DiscardRequests(reqs)

	// Tracks every "Handle" goroutine spawned for this connection so that we can
	// wait for them to finish before this function returns. Without this, channels
	// could still be in use after "sconn" is closed by the caller's deferred call.
	var wg sync.WaitGroup
	defer wg.Wait()

	for ch := range chans {
		// If not a session channel we just move on because it's not something we
		// know how to handle at this point.
		if ch.ChannelType() != "session" {
			_ = ch.Reject(ssh.UnknownChannelType, "unknown channel type")
			continue
		}

		channel, requests, err := ch.Accept()
		if err != nil {
			continue
		}

		go func(in <-chan *ssh.Request) {
			for req := range in {
				// Channels have a type that is dependent on the protocol. For SFTP
				// this is "subsystem" with a payload that (should) be "sftp". Discard
				// anything else we receive ("pty", "shell", etc)
				ok := req.Type == "subsystem" && len(req.Payload) >= 4 && string(req.Payload[4:]) == "sftp"
				_ = req.Reply(ok, nil)
			}
		}(requests)

		srv, ok := c.manager.Get(sconn.Permissions.Extensions["uuid"])
		if !ok {
			// No matching server instance for this connection's UUID: nothing will
			// ever consume this channel, so close it immediately to avoid leaking it.
			_ = channel.Close()
			continue
		}

		// Handle each channel concurrently so a slow or stuck client on one channel
		// (e.g. gvfs holding a session open) can't block the rest of the connection.
		wg.Add(1)
		go func(channel ssh.Channel) {
			defer wg.Done()
			if err := c.Handle(sconn, srv, channel); err != nil {
				_ = channel.Close()
				log.WithField("error", err).
					WithField("ip", conn.RemoteAddr().String()).
					Error("sftp: error handling channel")
			}
		}(channel)
	}
	return nil
}

// Handle spins up a SFTP server instance for the authenticated user's server allowing
// them access to the underlying filesystem.
func (c *SFTPServer) Handle(conn *ssh.ServerConn, srv *server.Server, channel ssh.Channel) error {
	handler, err := NewHandler(conn, srv)
	if err != nil {
		return errors.WithStackIf(err)
	}

	ctx := srv.Sftp().Context(handler.User())
	rs := sftp.NewRequestServer(channel, handler.Handlers())

	// Signals the supervisor goroutine below to stop watching "ctx" once this
	// function returns, so it doesn't linger until the server's protected-state
	// context is eventually cancelled (which may be much later, or never).
	done := make(chan struct{})
	defer close(done)

	go func() {
		select {
		case <-ctx.Done():
			srv.Log().WithField("user", conn.User()).Warn("sftp: terminating active session")
			_ = rs.Close()
		case <-done:
		}
	}()

	err = rs.Serve()
	_ = rs.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// Generates a new ED25519 private key that is used for host authentication when
// a user connects to the SFTP server.
func (c *SFTPServer) generateED25519PrivateKey() error {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return errors.Wrap(err, "sftp: failed to generate ED25519 private key")
	}
	if err := os.MkdirAll(path.Dir(c.PrivateKeyPath()), 0o755); err != nil {
		return errors.Wrap(err, "sftp: could not create internal sftp data directory")
	}
	o, err := os.OpenFile(c.PrivateKeyPath(), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return errors.WithStack(err)
	}
	defer o.Close()

	b, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return errors.Wrap(err, "sftp: failed to marshal private key into bytes")
	}
	if err := pem.Encode(o, &pem.Block{Type: "PRIVATE KEY", Bytes: b}); err != nil {
		return errors.Wrap(err, "sftp: failed to write ED25519 private key to disk")
	}
	return nil
}

func (c *SFTPServer) makeCredentialsRequest(conn ssh.ConnMetadata, t remote.SftpAuthRequestType, p string) (*ssh.Permissions, error) {
	request := remote.SftpAuthRequest{
		Type:          t,
		User:          conn.User(),
		Pass:          p,
		IP:            conn.RemoteAddr().String(),
		SessionID:     conn.SessionID(),
		ClientVersion: conn.ClientVersion(),
	}

	logger := log.WithFields(log.Fields{"subsystem": "sftp", "method": request.Type, "username": request.User, "ip": request.IP})
	logger.Debug("validating credentials for SFTP connection")

	if !validUsernameRegexp.MatchString(request.User) {
		logger.Warn("failed to validate user credentials (invalid format)")
		return nil, &remote.SftpInvalidCredentialsError{}
	}

	if t == remote.SftpAuthPassword && config.Get().System.Sftp.KeyOnly {
		logger.Warn("failed to validate user credentials (password authentication is disabled; only SSH keys are allowed)")
		return nil, &remote.SftpKeyOnlyError{}
	}

	resp, err := c.manager.Client().ValidateSftpCredentials(context.Background(), request)
	if err != nil {
		if _, ok := err.(*remote.SftpInvalidCredentialsError); ok {
			logger.Warn("failed to validate user credentials (invalid username or password)")
		} else {
			logger.WithField("error", err).Error("encountered an error while trying to validate user credentials")
		}
		return nil, err
	}

	logger.WithField("server", resp.Server).Debug("credentials validated and matched to server instance")

	// The Panel has said who this is and what they may do. Plugins get the
	// last word, for a policy the Panel cannot express, such as which
	// addresses are allowed to connect to this node.
	if plugins.HasSftpAuthHooks() {
		allow, reason := plugins.GateSftpAuth(api.SftpAuth{
			User:        resp.User,
			Username:    request.User,
			IP:          conn.RemoteAddr().String(),
			ServerUUID:  resp.Server,
			PublicKey:   t == remote.SftpAuthPublicKey,
			Permissions: resp.Permissions,
		})
		if !allow {
			logger.WithField("reason", reason).Warn("a plugin refused this sftp session")

			// Reported as invalid credentials rather than with the plugin's
			// reason. Anyone who can reach the SFTP port can try to log in, so
			// telling them why they were refused hands an attacker a probe for
			// whatever policy the plugin implements.
			return nil, &remote.SftpInvalidCredentialsError{}
		}
	}

	permissions := ssh.Permissions{
		Extensions: map[string]string{
			"ip":          conn.RemoteAddr().String(),
			"uuid":        resp.Server,
			"user":        resp.User,
			"permissions": strings.Join(resp.Permissions, ","),
		},
	}

	return &permissions, nil
}

// PrivateKeyPath returns the path the host private key for this server instance.
func (c *SFTPServer) PrivateKeyPath() string {
	return path.Join(c.BasePath, ".sftp/id_ed25519")
}
