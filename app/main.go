package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"syscall"
)

const tmpDirName = "mydocker"

type nopReader struct{}

func (nopReader) Read(p []byte) (n int, err error) {
	return 0, io.EOF
}

func copyExecutable(src, dst string) error {
	cmd := exec.Command("cp", "--parents", src, dst+"/")
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
	// defer os.RemoveAll(tmpDirName)

	command := os.Args[3]
	args := os.Args[4:len(os.Args)]

	if err := copyExecutable(command, tmpDirName); err != nil {
		panic(fmt.Sprintf("failed to copy command to tmp dir: %v", err))
	}

	if err := syscall.Chroot(tmpDirName); err != nil {
		panic(fmt.Sprintf("failed to chroot: %v", err))
	}
	if err := os.Chdir("/"); err != nil {
		panic(fmt.Sprintf("failed to chdir: %v", err))
	}

	cmd := exec.Command(command, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = nopReader{}

	if err := cmd.Run(); err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			os.Exit(exitError.ExitCode())
		}
	}
}
