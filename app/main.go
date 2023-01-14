package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"syscall"
)

const tmpDirName = "mydocker"

type nopWriter struct{}

func (nopWriter) Write(p []byte) (n int, err error) {
	return 0, nil
}

func copyExecutable(src, dst string) error {
	cmd := exec.Command("cp", "-r", src, dst+"/")
	return cmd.Run()
}

// Usage: your_docker.sh run <image> <command> <arg1> <arg2> ...
func main() {
	defer func() {
		if p := recover(); p != nil {
			log.Fatal(p)
		}
	}()

	if err := os.Mkdir(tmpDirName, 700); err != nil {
		panic(fmt.Sprintf("failed to create tmp dir: %v", err))
	}
	defer os.RemoveAll(tmpDirName)

	command := os.Args[3]
	args := os.Args[4:len(os.Args)]

	if err := copyExecutable(command, tmpDirName); err != nil {
		panic(fmt.Sprintf("failed to copy command to tmp dir: %v", err))
	}

	if err := syscall.Chroot(tmpDirName); err != nil {
		panic(fmt.Sprintf("failed to chroot: %v", err))
	}

	cmd := exec.Command(command, args...)
	cmd.Stdout = nopWriter{}
	cmd.Stderr = nopWriter{}

	if err := cmd.Run(); err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			os.Exit(exitError.ExitCode())
		}
	}
}
