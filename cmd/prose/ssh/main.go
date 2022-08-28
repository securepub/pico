package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"git.sr.ht/~erock/pico/db/postgres"
	"git.sr.ht/~erock/pico/filehandlers"
	"git.sr.ht/~erock/pico/imgs/storage"
	"git.sr.ht/~erock/pico/prose"
	"git.sr.ht/~erock/pico/shared"
	"git.sr.ht/~erock/pico/wish/cms"
	"git.sr.ht/~erock/pico/wish/list"
	"git.sr.ht/~erock/pico/wish/pipe"
	"git.sr.ht/~erock/pico/wish/proxy"
	"git.sr.ht/~erock/pico/wish/send/auth"
	wishrsync "git.sr.ht/~erock/pico/wish/send/rsync"
	"git.sr.ht/~erock/pico/wish/send/scp"
	"git.sr.ht/~erock/pico/wish/send/sftp"
	"github.com/charmbracelet/promwish"
	"github.com/charmbracelet/wish"
	bm "github.com/charmbracelet/wish/bubbletea"
	lm "github.com/charmbracelet/wish/logging"
	"github.com/gliderlabs/ssh"
)

type SSHServer struct{}

func (me *SSHServer) authHandler(ctx ssh.Context, key ssh.PublicKey) bool {
	return true
}

func createRouter(handler *filehandlers.ScpUploadHandler) proxy.Router {
	return func(sh ssh.Handler, s ssh.Session) []wish.Middleware {
		return []wish.Middleware{
			pipe.Middleware(handler, ".md"),
			list.Middleware(handler),
			scp.Middleware(handler),
			wishrsync.Middleware(handler),
			bm.Middleware(cms.Middleware(&handler.Cfg.ConfigCms, handler.Cfg)),
			lm.Middleware(),
			auth.Middleware(handler),
		}
	}
}

func withProxy(handler *filehandlers.ScpUploadHandler, otherMiddleware ...wish.Middleware) ssh.Option {
	return func(server *ssh.Server) error {
		err := sftp.SSHOption(handler)(server)
		if err != nil {
			return err
		}

		return proxy.WithProxy(createRouter(handler), otherMiddleware...)(server)
	}
}

func main() {
	host := shared.GetEnv("PROSE_HOST", "0.0.0.0")
	port := shared.GetEnv("PROSE_SSH_PORT", "2222")
	promPort := shared.GetEnv("PROSE_PROM_PORT", "9222")
	cfg := prose.NewConfigSite()
	logger := cfg.Logger
	dbh := postgres.NewDB(&cfg.ConfigCms)
	defer dbh.Close()
	hooks := &prose.MarkdownHooks{
		Cfg: cfg,
		Db:  dbh,
	}

	var st storage.ObjectStorage
	var err error
	if cfg.MinioURL == "" {
		st, err = storage.NewStorageFS(cfg.StorageDir)
	} else {
		st, err = storage.NewStorageMinio(cfg.MinioURL, cfg.MinioUser, cfg.MinioPass)
	}

	if err != nil {
		logger.Fatal(err)
	}

	handler := filehandlers.NewScpPostHandler(dbh, cfg, hooks, st)

	sshServer := &SSHServer{}
	s, err := wish.NewServer(
		wish.WithAddress(fmt.Sprintf("%s:%s", host, port)),
		wish.WithHostKeyPath("ssh_data/term_info_ed25519"),
		wish.WithPublicKeyAuth(sshServer.authHandler),
		withProxy(
			handler,
			promwish.Middleware(fmt.Sprintf("%s:%s", host, promPort), "prose-ssh"),
		),
	)
	if err != nil {
		logger.Fatal(err)
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	logger.Infof("Starting SSH server on %s:%s", host, port)
	go func() {
		if err = s.ListenAndServe(); err != nil {
			logger.Fatal(err)
		}
	}()

	<-done
	logger.Info("Stopping SSH server")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer func() { cancel() }()
	if err := s.Shutdown(ctx); err != nil {
		logger.Fatal(err)
	}
}
