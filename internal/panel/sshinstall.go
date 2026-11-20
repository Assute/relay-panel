// language: Go 1.23, file: internal/panel/sshinstall.go
// 通过 SSH 把节点装到一台新服务器上。
//
// 为什么要有这个：现在加一台节点是「面板生成命令 → 复制 → 登服务器 →
// 粘贴 → 等装完」，十台机器就是十遍。这里用密码登一次，剩下全自动。
//
// 只做密码认证，不碰密钥：面板要么不存服务器密钥，要么就得管一堆私钥文件，
// 对自用面板来说密码 + 主机指纹校验更划算。
package panel

import (
	"bytes"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

type sshTarget struct {
	Host    string
	Port    int
	User    string
	Pass    string
	HostKey string // 已记录的指纹，空表示首次连接
}

// runInstallScript 登录目标机，把安装脚本从标准输入喂给 bash。
//
// 不走 sftp 是有意的：x/crypto/ssh 自带 session 的 stdin，
// 少引一个 sftp 依赖，也少一处要处理权限的地方。
func runInstallScript(t sshTarget, script, scriptArgs string, timeout time.Duration) (output, hostKey string, err error) {
	var fp string
	cfg := &ssh.ClientConfig{
		User:    t.User,
		Auth:    []ssh.AuthMethod{ssh.Password(t.Pass)},
		Timeout: 15 * time.Second,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			fp = ssh.FingerprintSHA256(key)
			if t.HostKey != "" && t.HostKey != fp {
				return fmt.Errorf("主机指纹变了（记录的 %s，实际 %s），可能不是原来那台", t.HostKey, fp)
			}
			return nil
		},
	}

	addr := net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return "", fp, fmt.Errorf("连接 %s 失败: %w", addr, err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return "", fp, fmt.Errorf("打开会话失败: %w", err)
	}
	defer sess.Close()

	var buf bytes.Buffer
	sess.Stdout = &buf
	sess.Stderr = &buf
	sess.Stdin = strings.NewReader(script)

	done := make(chan error, 1)
	go func() { done <- sess.Run("bash -s -- " + scriptArgs) }()

	select {
	case runErr := <-done:
		out := buf.String()
		if runErr != nil {
			// 脚本自己会打印失败原因，把它带回去比只回一句 exit status 1 有用
			return out, fp, fmt.Errorf("安装脚本退出异常: %w", runErr)
		}
		return out, fp, nil
	case <-time.After(timeout):
		_ = sess.Close()
		return buf.String(), fp, fmt.Errorf("安装超时（%s），远端可能还在跑", timeout)
	}
}
