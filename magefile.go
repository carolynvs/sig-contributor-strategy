// +build mage

// This is a magefile, and is a "makefile for go".
// See https://magefile.org/
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/carolynvs/magex/xplat"

	"github.com/carolynvs/magex/pkg"
	"github.com/carolynvs/magex/shx"
	"github.com/magefile/mage/mg"
	"github.com/magefile/mage/sh"
	"github.com/pkg/errors"
)

// Default target to run when none is specified
// If not set, running mage will list available targets
var (
	Default        = Preview
	contributeRepo = "../contribute"
)

const (
	containerName = "sig-contributor-strategy"
	img           = containerName
)

// Ensure Mage is installed and on the PATH.
func EnsureMage() error {
	return pkg.EnsureMage("")
}

// Compile the website to website/public.
func Build() error {
	mg.Deps(clean, buildImage)

	// Build the volume mount for a local contribute repo, if present
	contribMount, goModMount, err := useLocalContributeModule()
	if err != nil {
		return err
	}

	pwd, _ := os.Getwd()
	return sh.RunV("docker", shx.CollapseArgs("run", "--rm", "-v", pwd+":/src",
		contribMount, goModMount, containerName, "--debug", "--verbose")...)
}

// Run a local server to preview the website and watch for changes.
func Preview() error {
	mg.Deps(clean, buildImage)

	// Build the volume mount for a local contribute repo, if present
	contribMount, goModMount, err := useLocalContributeModule()
	if err != nil {
		return err
	}

	port := getPort()
	pwd, _ := os.Getwd()
	err = sh.RunV("docker", shx.CollapseArgs("run", "-d", "-v", pwd+":/src",
		contribMount, goModMount, "-p", port+":1313",
		"--name", containerName, img, "server", "--debug", "--verbose",
		"--buildDrafts", "--buildFuture", "--noHTTPCache", "--watch", "--bind=0.0.0.0")...)
	if err != nil {
		return errors.Wrap(err, "could not run website container")
	}

	err = awaitContainer(containerName, "Web Server is available")
	if err != nil {
		return errors.Wrap(err, "error waiting for the website to become ready")
	}

	url := "http://localhost:" + getPort()
	return errors.Wrap(openURL(url), "could not open the website in a browser")
}

func Logs() error {
	return shx.RunE("docker", "logs", "-f", img)
}

// Use hugo in a docker container.
func Hugo() error {
	pwd, _ := os.Getwd()
	cmd := sh.Command("docker", "run", "--rm", "-it", "-v", pwd+":/src", "-w", "/src/website", img, "shell").
		Stdout(os.Stdout)
	cmd.Cmd.Stdin = os.Stdin
	_, _, err := cmd.Run()
	return errors.Wrap(err, "could not start hugo in a container")
}

// Create go.local.mod with any appropriate replace statements, and
// returns the local contribute mount flag if present.
func useLocalContributeModule() (contribMount string, goModMount string, err error) {
	if repo, ok := os.LookupEnv("CONTRIBUTE_REPO"); ok {
		contributeRepo = repo
	}
	contributeRepo, _ := filepath.Abs(contributeRepo)

	if mg.Verbose() {
		fmt.Println("Checking for a local copy of github.com/cncf/contribute at", contributeRepo)
	}

	// Only mount the local repo if it exists, otherwise use the one on github
	_, err = os.Stat(contributeRepo)
	if err != nil {
		return "", "", nil
	}

	log.Println("Using your local copy of github.com/cncf/contribute ->", contributeRepo)
	pwd, _ := os.Getwd()
	localGoMod := filepath.Join(pwd, "website/go.local.mod")
	err = copyFile("website/go.mod", localGoMod)
	if err != nil {
		return "", "", err
	}
	goModMount = fmt.Sprintf("-v=%s:/src/website/go.mod", localGoMod)

	err = sh.RunV("docker", "run", "--rm", "--entrypoint", "go",
		"-v", pwd+":/src", goModMount, "-w", "/src/website", img,
		"mod", "edit", "-replace", "github.com/cncf/contribute=/src/contribute")
	if err != nil {
		return "", "", errors.Wrap(err, "could not modify go.mod to use your local copy of github.com/cncf/contribute")
	}

	contribMount = fmt.Sprintf("-v=%s:/src/contribute", contributeRepo)
	return contribMount, goModMount, nil
}

func copyFile(src string, dest string) error {
	s, err := os.Open(src)
	if err != nil {
		return errors.Wrapf(err, "could not open %s", src)
	}

	d, err := os.Create(dest)
	if err != nil {
		return errors.Wrapf(err, "could not create %s", dest)
	}

	_, err = io.Copy(d, s)
	return errors.Wrapf(err, "could not copy %s to %s", src, dest)
}

func openURL(url string) error {
	shell := xplat.DetectShell()
	if shell == "msystem" {
		return shx.RunE("cmd", "/C", "open "+url)
	}
	if runtime.GOOS == "windows" {
		return shx.RunE("powershell", "Start-Process", url)
	}
	return shx.RunE(shell, "-c", "open "+url)
}

func getPort() string {
	port := os.Getenv("PORT")
	if port == "" {
		port = "1313"
	}
	return port
}

func buildImage() error {
	mg.Deps(docsy)
	err := shx.RunE("docker", "build", "-t", img,
		"-f", "website/Dockerfile", ".")
	return errors.Wrap(err, "could not build website image")
}

func docsy() error {
	_, err := os.Stat("website/themes/docsy")
	if err != nil {
		if os.IsNotExist(err) {
			return shx.RunE("git", "submodule", "update", "--init", "--recursive", "--force")
		}
		return errors.Wrap(err, "could not clone the docsy theme")
	}

	return nil
}

func containerExists(name string) bool {
	output, err := sh.Output("docker", "ps", "--all", "--filter", "name="+name)
	return err == nil && strings.Contains(output, name)
}

func removeContainer(name string) error {
	return sh.RunV("docker", "rm", "-f", name)
}

func awaitContainer(name string, logSearch string) error {
	cxt, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	for {
		select {
		case <-cxt.Done():
			return errors.Errorf("timeout waiting for container %s to become ready", name)
		default:
			logs, err := sh.Output("docker", "logs", name)
			if err != nil {
				return errors.Wrapf(err, "could not get logs for container %s", name)
			}

			if strings.Contains(logs, logSearch) {
				return nil
			}

			if mg.Verbose() {
				fmt.Println(logs)
			}

			time.Sleep(time.Second)
		}
	}
}

func clean() error {
	err := os.RemoveAll("website/public")
	if err != nil {
		return errors.Wrap(err, "could not remove website/public")
	}

	if containerExists(containerName) {
		return removeContainer(containerName)
	}

	return nil
}