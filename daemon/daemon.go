// Copyright 2019 Drone.IO Inc. All rights reserved.
// Use of this source code is governed by the Polyform License
// that can be found in the LICENSE file.

// Package daemon implements the daemon runner.

package daemon

import (
	"context"
	"time"

	"github.com/drone-runners/drone-runner-exec/engine"
	"github.com/drone-runners/drone-runner-exec/engine/resource"
	"github.com/drone-runners/drone-runner-exec/internal/match"
	"github.com/drone-runners/drone-runner-exec/runtime"

	"github.com/drone/runner-go/client"
	"github.com/drone/runner-go/handler/router"
	"github.com/drone/runner-go/logger"
	loghistory "github.com/drone/runner-go/logger/history"
	"github.com/drone/runner-go/pipeline/history"
	"github.com/drone/runner-go/pipeline/remote"
	"github.com/drone/runner-go/secret"
	"github.com/drone/runner-go/server"

	"github.com/orandin/lumberjackrus"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

// Run runs the service and blocks until complete.
func Run(parentCtx context.Context, config Config) error {
	ctx, cancelBySingleStageMode := context.WithCancel(parentCtx)
	defer cancelBySingleStageMode()
	isScheduledExit := false

	setupLogger(config)

	cli := client.New(
		config.Client.Address,
		config.Client.Secret,
		config.Client.SkipVerify,
	)
	if config.Client.Dump {
		cli.Dumper = logger.StandardDumper(
			config.Client.DumpBody,
		)
	}
	cli.Logger = logger.Logrus(
		logrus.NewEntry(
			logrus.StandardLogger(), // TODO(bradrydzewski) get from context
		),
	)

	engine := engine.New()
	remote := remote.New(cli)
	tracer := history.New(remote)
	hook := loghistory.New()
	logrus.AddHook(hook)

	poller := &runtime.Poller{
		Client: cli,
		Runner: &runtime.Runner{
			Client:   cli,
			Environ:  config.Runner.Environ,
			Machine:  config.Runner.Name,
			Root:     config.Runner.Root,
			Symlinks: config.Runner.Symlinks,
			Reporter: tracer,
			Match: match.Func(
				config.Limit.Repos,
				config.Limit.Events,
				config.Limit.Trusted,
			),
			Secret: secret.External(
				config.Secret.Endpoint,
				config.Secret.Token,
				config.Secret.SkipVerify,
			),
			Execer: runtime.NewExecer(
				tracer,
				remote,
				engine,
				config.Runner.Procs,
			),
		},
		Filter: &client.Filter{
			Kind:    resource.Kind,
			Type:    resource.Type,
			OS:      config.Platform.OS,
			Arch:    config.Platform.Arch,
			Variant: config.Platform.Variant,
			Kernel:  config.Platform.Kernel,
			Labels:  config.Runner.Labels,
		},
	}

	var g errgroup.Group
	if config.Server.Port != "false" {
		server := server.Server{
			Addr: config.Server.Port,
			Handler: router.New(tracer, hook, router.Config{
				Username: config.Dashboard.Username,
				Password: config.Dashboard.Password,
				Realm:    config.Dashboard.Realm,
			}),
		}

		logrus.WithField("addr", config.Server.Port).
			Infoln("starting the server")

		g.Go(func() error {
			return server.ListenAndServe(ctx)
		})
	} else {
		logrus.
			Warnln("server is disable due to port value '0'")
	}

	// Ping the server and block until a successful connection
	// to the server has been established.
	for {
		err := cli.Ping(ctx, config.Runner.Name)
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			logrus.WithError(err).
				Errorln("cannot ping the remote server")
			time.Sleep(time.Second)
		} else {
			logrus.Infoln("successfully pinged the remote server")
			break
		}
	}

	g.Go(func() error {
		logrus.WithField("capacity", config.Runner.Capacity).
			WithField("endpoint", config.Client.Address).
			WithField("kind", resource.Kind).
			WithField("type", resource.Type).
			Infoln("polling the remote server")

		isSingleStageMode := config.Runner.Capacity < 1

		if isSingleStageMode {
			pollErr := poller.Poll(ctx, 0)
			if pollErr != nil {
				logrus.WithError(pollErr).
					Errorln("failure")
			} else {
				// Poller return nil only when successfully finished single stage
				isScheduledExit = true
			}
			cancelBySingleStageMode()
			return nil
		}

		poller.Poll(ctx, config.Runner.Capacity)
		return nil
	})

	err := g.Wait()

	if err != nil && err == context.Canceled && isScheduledExit {
		logrus.Infoln("shutting down the server (scheduled)")
		return nil
	}

	if err != nil {
		logrus.WithError(err).
			Errorln("shutting down the server")
	}
	return err
}

// helper function configures the global logger from
// the loaded configuration.
func setupLogger(config Config) error {
	logger.Default = logger.Logrus(
		logrus.NewEntry(
			logrus.StandardLogger(),
		),
	)
	if config.Debug {
		logrus.SetLevel(logrus.DebugLevel)
	}
	if config.Trace {
		logrus.SetLevel(logrus.TraceLevel)
	}
	if config.Logger.File == "" {
		return nil
	}
	hook, err := lumberjackrus.NewHook(
		&lumberjackrus.LogFile{
			Filename:   config.Logger.File,
			MaxSize:    config.Logger.MaxSize,
			MaxBackups: config.Logger.MaxBackups,
			MaxAge:     config.Logger.MaxAge,
		},
		logrus.TraceLevel,
		&logrus.TextFormatter{},
		nil,
	)
	if err != nil {
		return err
	}
	logrus.AddHook(hook)
	return nil
}
