package filewatch

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

const DefaultDebounce = 100 * time.Millisecond

type Callbacks struct {
	Reload        func() error
	OnReloaded    func()
	OnReloadError func(error)
	OnWatchError  func(error)
}

func Watch(ctx context.Context, path string, debounce time.Duration, onChange func(), onWatchError func(error)) error {
	return Run(ctx, nil, path, debounce, Callbacks{
		Reload: func() error {
			onChange()
			return nil
		},
		OnWatchError: onWatchError,
	})
}

func Run(ctx context.Context, stop <-chan struct{}, path string, debounce time.Duration, callbacks Callbacks) error {
	if callbacks.Reload == nil {
		return errors.New("reload callback is required")
	}
	if debounce < 0 {
		debounce = 0
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	cleanPath := filepath.Clean(path)
	dir := filepath.Dir(cleanPath)
	base := filepath.Base(cleanPath)
	if err := watcher.Add(dir); err != nil {
		return err
	}

	var (
		timer   *time.Timer
		timerCh <-chan time.Time
	)
	stopTimer := func() {
		if timer == nil {
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timerCh = nil
	}
	scheduleReload := func() {
		if debounce == 0 {
			if err := callbacks.Reload(); err != nil {
				if callbacks.OnReloadError != nil {
					callbacks.OnReloadError(err)
				}
			} else if callbacks.OnReloaded != nil {
				callbacks.OnReloaded()
			}
			return
		}
		if timer == nil {
			timer = time.NewTimer(debounce)
		} else {
			stopTimer()
			timer.Reset(debounce)
		}
		timerCh = timer.C
	}
	defer stopTimer()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-stop:
			return nil
		case <-timerCh:
			timerCh = nil
			if err := callbacks.Reload(); err != nil {
				if callbacks.OnReloadError != nil {
					callbacks.OnReloadError(err)
				}
			} else if callbacks.OnReloaded != nil {
				callbacks.OnReloaded()
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			if err != nil && callbacks.OnWatchError != nil {
				callbacks.OnWatchError(err)
			}
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if shouldReload(base, event) {
				scheduleReload()
			}
		}
	}
}

func shouldReload(base string, event fsnotify.Event) bool {
	if filepath.Base(event.Name) != base {
		return false
	}
	return event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Remove|fsnotify.Chmod) != 0
}
