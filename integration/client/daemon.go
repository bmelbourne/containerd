/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/plugin"
)

type daemon struct {
	sync.Mutex
	addr string
	cmd  *exec.Cmd
}

func (d *daemon) start(name, address string, args []string, stdout, stderr io.Writer) error {
	d.Lock()
	defer d.Unlock()
	if d.cmd != nil {
		return errors.New("daemon is already running")
	}
	args = append(args, []string{"--address", address}...)
	cmd := exec.Command(name, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		cmd.Wait()
		return fmt.Errorf("failed to start daemon: %w", err)
	}
	d.addr = address
	d.cmd = cmd
	return nil
}

func (d *daemon) waitForStart(ctx context.Context) (*client.Client, error) {
	var (
		clientInstance *client.Client
		serving        bool
		err            error
		ticker         = time.NewTicker(500 * time.Millisecond)
	)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			clientInstance, err = client.New(d.addr)
			if err != nil {
				continue
			}
			serving, err = clientInstance.IsServing(ctx)
			if !serving {
				clientInstance.Close()
				if err == nil {
					err = errors.New("connection was successful but service is not available")
				}
				continue
			}
			resp, perr := clientInstance.IntrospectionService().Plugins(ctx)
			if perr != nil {
				return nil, fmt.Errorf("failed to get plugin list: %w", perr)
			}
			var loadErr error
			for _, p := range resp.Plugins {
				if p.InitErr != nil && !strings.Contains(p.InitErr.Message, plugin.ErrSkipPlugin.Error()) {
					pluginErr := fmt.Errorf("failed to load %s.%s: %s", p.Type, p.ID, p.InitErr.Message)
					loadErr = errors.Join(loadErr, pluginErr)
				}
			}
			if loadErr != nil {
				return nil, loadErr
			}

			return clientInstance, err
		case <-ctx.Done():
			return nil, fmt.Errorf("context deadline exceeded: %w", err)
		}
	}
}

func (d *daemon) Stop() error {
	d.Lock()
	defer d.Unlock()
	if d.cmd == nil {
		return errors.New("daemon is not running")
	}
	return d.cmd.Process.Signal(syscall.SIGTERM)
}

func (d *daemon) Kill() error {
	d.Lock()
	defer d.Unlock()
	if d.cmd == nil {
		return errors.New("daemon is not running")
	}
	return d.cmd.Process.Kill()
}

func (d *daemon) Wait() error {
	d.Lock()
	defer d.Unlock()
	if d.cmd == nil {
		return errors.New("daemon is not running")
	}
	err := d.cmd.Wait()
	d.cmd = nil
	return err
}

func (d *daemon) Restart(stopCb func()) error {
	d.Lock()
	defer d.Unlock()
	if d.cmd == nil {
		return errors.New("daemon is not running")
	}

	signal := syscall.SIGTERM
	if runtime.GOOS == "windows" {
		signal = syscall.SIGKILL
	}
	var err error
	if err = d.cmd.Process.Signal(signal); err != nil {
		return fmt.Errorf("failed to signal daemon: %w", err)
	}

	d.cmd.Wait()

	if stopCb != nil {
		stopCb()
	}

	cmd := exec.Command(d.cmd.Path, d.cmd.Args[1:]...)
	cmd.Stdout = d.cmd.Stdout
	cmd.Stderr = d.cmd.Stderr
	if err := cmd.Start(); err != nil {
		cmd.Wait()
		return fmt.Errorf("failed to start new daemon instance: %w", err)
	}
	d.cmd = cmd

	return nil
}
