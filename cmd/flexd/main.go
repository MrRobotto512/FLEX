// flexd runs the FLEX server headless. The desktop app embeds the same
// server package; this binary is for development and for machines without
// a desktop.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"

	"flex/server"
	"flex/web"
)

func main() {
	debug := flag.Bool("debug", false, "verbose logging (ffmpeg output, every request)")
	flag.Parse()

	_ = godotenv.Load() // .env is optional
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := server.FromEnv()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	// first run on a Windows PC without FFmpeg: fetch it (checked copy) first
	if c := cfg; c.Validate() == nil && !c.FFmpegAvailable() && server.CanDownloadFFmpeg() && cfg.FFmpeg == "" {
		log.Info("ffmpeg not found, downloading it (about 260 MB, once)", "to", cfg.DataDir)
		last := -1
		err := server.EnsureFFmpeg(context.Background(), cfg.DataDir, func(done, total int64) {
			if pct := int(done * 100 / max(total, 1)); pct/10 != last/10 {
				last = pct
				log.Info("ffmpeg download", "percent", pct)
			}
		})
		if err != nil {
			log.Error("ffmpeg", "err", err)
			os.Exit(1)
		}
	}
	srv, err := server.New(cfg, log, web.Files)
	if err != nil {
		log.Error("start", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx); err != nil {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}
