package main

import (
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"pimg/internal/carrier"
	"pimg/internal/cryptox"
)

// This command deliberately runs before config/DB/session initialization.
func recoverFile(args []string, log io.Writer) (retErr error) {
	flags := flag.NewFlagSet("recover-file", flag.ContinueOnError)
	flags.SetOutput(log)
	keyPath := flags.String("key-file", "", "原主密钥文件（base64）")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *keyPath == "" || flags.NArg() != 2 {
		return errors.New("用法: pimg recover-file --key-file <主密钥文件> <加密载体> <输出文件>")
	}
	encoded, err := os.ReadFile(*keyPath)
	if err != nil {
		return errors.New("无法读取主密钥文件")
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	clear(encoded)
	if err != nil {
		return errors.New("主密钥文件不是有效的 base64")
	}
	master, err := cryptox.NewMaster(key)
	if err != nil {
		clear(key)
		return err
	}
	defer clear(key)
	input, err := os.Open(flags.Arg(0))
	if err != nil {
		return err
	}
	defer input.Close()
	// Never overwrite another file, including the carrier or the key itself.
	output, err := os.OpenFile(flags.Arg(1), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() {
		output.Close()
		if retErr != nil {
			_ = os.Remove(flags.Arg(1))
		}
	}()
	meta, err := carrier.Recover(input, output, master)
	if err != nil {
		return err
	}
	if err = output.Sync(); err != nil {
		return err
	}
	if err = output.Close(); err != nil {
		return err
	}
	fmt.Fprintf(log, "恢复成功：%d 字节，SHA256 %s\n", meta.Size, meta.SHA256)
	return nil
}
