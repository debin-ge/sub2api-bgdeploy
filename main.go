package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"
)

// Version is injected by -ldflags "-X main.Version=...".
var Version = "dev"

func main() {
	if err := runCLI(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
}

func runCLI(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("bgdeploy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configFile := fs.String("config", "", "主机级运行配置文件（默认当前目录/runtime.yaml）")
	root := fs.String("root", "", "部署根目录（默认当前工作目录）")
	nginxDir := fs.String("nginx-dir", "", "nginx 蓝绿配置目录")
	nginxSnippetDir := fs.String("nginx-snippet-dir", "", "nginx snippet 目录")
	fs.Usage = func() {}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(stdout)
			return nil
		}
		printUsage(stderr)
		return err
	}

	rest := fs.Args()
	if len(rest) == 0 {
		printUsage(stdout)
		return errors.New("缺少命令")
	}
	command, commandArgs := rest[0], rest[1:]
	if command == "help" || command == "-h" || command == "--help" {
		if len(commandArgs) != 0 {
			return usageError("help")
		}
		printUsage(stdout)
		return nil
	}
	if len(commandArgs) == 1 && (commandArgs[0] == "-h" || commandArgs[0] == "--help") {
		printUsage(stdout)
		return nil
	}
	if command == "version" {
		fmt.Fprintf(stdout, "bgdeploy %s\n", Version)
		return nil
	}

	app, err := newAppWithConfig(*configFile, *root, *nginxDir, *nginxSnippetDir, stdout, stderr)
	if err != nil {
		return err
	}

	switch command {
	case "bootstrap":
		if len(commandArgs) != 0 {
			return usageError("bootstrap")
		}
		return app.bootstrap()
	case "check":
		if len(commandArgs) != 0 {
			return usageError("check")
		}
		app.requireRoot = true
		return app.checkDependencies(ctx)
	case "render":
		if len(commandArgs) != 0 {
			return usageError("render")
		}
		return app.render(ctx)
	case "init":
		if len(commandArgs) != 1 {
			return usageError("init <slug>")
		}
		return app.initStack(ctx, commandArgs[0])
	case "deploy":
		if len(commandArgs) < 1 || len(commandArgs) > 2 {
			return usageError("deploy <slug> [image-tag]")
		}
		tag := ""
		if len(commandArgs) == 2 {
			tag = commandArgs[1]
		}
		return app.deploy(ctx, commandArgs[0], tag)
	case "rollback":
		if len(commandArgs) != 1 {
			return usageError("rollback <slug>")
		}
		return app.rollback(ctx, commandArgs[0])
	case "status":
		if len(commandArgs) > 1 {
			return usageError("status [slug]")
		}
		slug := ""
		if len(commandArgs) == 1 {
			slug = commandArgs[0]
		}
		return app.status(ctx, slug)
	case "teardown":
		if len(commandArgs) != 2 {
			return usageError("teardown <slug> <blue|green>")
		}
		return app.teardown(ctx, commandArgs[0], commandArgs[1])
	case "__drain":
		if len(commandArgs) != 3 {
			return usageError("__drain <seconds> <slug> <blue|green>")
		}
		seconds, err := strconv.Atoi(commandArgs[0])
		if err != nil || seconds < 0 {
			return fmt.Errorf("非法排空秒数: %q", commandArgs[0])
		}
		time.Sleep(time.Duration(seconds) * time.Second)
		err = app.teardown(ctx, commandArgs[1], commandArgs[2])
		_ = os.Remove(app.drainPIDFile(commandArgs[1], commandArgs[2]))
		return err
	default:
		printUsage(stderr)
		return fmt.Errorf("未知命令: %s", command)
	}
}

func usageError(usage string) error {
	return fmt.Errorf("用法: bgdeploy %s", usage)
}
