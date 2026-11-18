// Command llm-memory-gui is the desktop explorer of an llm-memory database.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"

	"github.com/Morditux/llm-memory/gui"
	"github.com/Morditux/llm-memory/gui/internal/view"
	"github.com/Morditux/llm-memory/internal/bootstrap"
	"github.com/Morditux/llm-memory/internal/config"
	"github.com/Morditux/llm-memory/internal/explorer"
)

const actor = "gui"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "llm-memory-gui:", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("llm-memory-gui", flag.ContinueOnError)
	flags := config.Register(fs)
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	st, cfg, err := bootstrap.OpenStore(ctx, flags, config.Env{
		Getenv:      os.Getenv,
		UserHomeDir: os.UserHomeDir,
		Getwd:       os.Getwd,
	})
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	eng, err := bootstrap.NewEngine(st, cfg, logger)
	if err != nil {
		return err
	}
	var searcher explorer.Searcher
	if eng != nil {
		searcher = eng
	}

	a := app.NewWithID("io.github.morditux.llm-memory")
	a.SetIcon(fyne.NewStaticResource("Icon.png", gui.Icon))
	w := view.NewMainWindow(ctx, a, explorer.New(st, searcher, actor))
	w.Window.SetMaster()
	go func() {
		<-ctx.Done()
		fyne.Do(a.Quit)
	}()
	w.Window.ShowAndRun()
	return nil
}
