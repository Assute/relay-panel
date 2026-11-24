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
	"log"
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
//
// 如果面板里嵌了对应架构的节点二进制，会先推上去再用它装；
// 没嵌就退回老路——让目标机自己去面板的 /dl/ 下载。
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

	// 先问一句架构，能不能推二进制看这个
	arch := detectArch(client)
	if bin := nodeBinary(arch); bin != nil {
		if err := pushBinary(client, bin, "/tmp/relay-node.bin"); err != nil {
			return "", fp, fmt.Errorf("推送节点二进制失败: %w", err)
		}
	} else if arch != "" {
		log.Printf("远程安装: 面板里没有 %s 的节点二进制，改为让目标机自行下载", arch)
	}

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

// detectArch 用 uname -m 认出目标机架构，映射成我们二进制文件的命名。
// 认不出来返回空串——那就老老实实走下载。
func detectArch(client *ssh.Client) string {
	sess, err := client.NewSession()
	if err != nil {
		return ""
	}
	defer sess.Close()
	var b bytes.Buffer
	sess.Stdout = &b
	if err := sess.Run("uname -m"); err != nil {
		return ""
	}
	switch strings.TrimSpace(b.String()) {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	case "armv7l", "armv7", "armv6l":
		return "arm"
	}
	return ""
}

// pushBinary 把本地文件按字节推进远端 /tmp。
// 用 cat 接 stdin，比 base64 省一整轮膨胀，也不用 sftp。
func pushBinary(client *ssh.Client, data []byte, dest string) error {
	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	sess.Stdin = bytes.NewReader(data)
	var errBuf bytes.Buffer
	sess.Stderr = &errBuf
	if err := sess.Run("cat > " + dest + " && chmod +x " + dest); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(errBuf.String()))
	}
	return nil
}

// updateScript 只换二进制，配置和账本一律不碰。
//
// 和安装脚本的区别就在这：安装要重写 /etc/relay/config.json、要清
// /var/lib/relay/key（那是重新注册）；更新是「同一个节点换新程序」，
// 凭证、累计用量、网卡状态都得留着，否则等于把账抹了重来。
const updateScript = `#!/bin/bash
set -euo pipefail
trap 'echo "[错误] 第 $LINENO 行失败: $BASH_COMMAND" >&2' ERR

BIN=/usr/local/bin/relay-node
if [ ! -s /tmp/relay-node.bin ]; then
  echo "没有收到新二进制" >&2
  exit 1
fi
SZ=$(wc -c < /tmp/relay-node.bin)
echo "[1/3] 收到新二进制（${SZ} 字节）"

if systemctl is-active --quiet relay-node 2>/dev/null; then
  echo "[2/3] 停止 relay-node"
  systemctl stop relay-node
else
  echo "[2/3] relay-node 没在跑，直接替换"
fi
mv -f /tmp/relay-node.bin "$BIN"
chmod +x "$BIN"

echo "[3/3] 启动"
systemctl start relay-node
sleep 2
if systemctl is-active --quiet relay-node; then
  echo "更新完成，当前版本："
  journalctl -u relay-node -n 6 --no-pager | grep -m1 '节点端版本' || true
else
  echo "启动失败，最近日志：" >&2
  journalctl -u relay-node -n 20 --no-pager >&2 || true
  exit 1
fi
`

// runNodeUpdate 把面板里的节点二进制推到目标机并重启服务。
func runNodeUpdate(t sshTarget, timeout time.Duration) (output, hostKey string, err error) {
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

	arch := detectArch(client)
	bin := nodeBinary(arch)
	if bin == nil {
		return "", fp, fmt.Errorf("面板里没有 %s 的节点二进制，无法更新（构建时要跑 make assets）", arch)
	}
	if err := pushBinary(client, bin, "/tmp/relay-node.bin"); err != nil {
		return "", fp, fmt.Errorf("推送二进制失败: %w", err)
	}

	sess, err := client.NewSession()
	if err != nil {
		return "", fp, fmt.Errorf("打开会话失败: %w", err)
	}
	defer sess.Close()

	var buf bytes.Buffer
	sess.Stdout = &buf
	sess.Stderr = &buf
	sess.Stdin = strings.NewReader(updateScript)

	done := make(chan error, 1)
	go func() { done <- sess.Run("bash -s") }()

	select {
	case runErr := <-done:
		out := buf.String()
		if runErr != nil {
			return out, fp, fmt.Errorf("更新脚本退出异常: %w", runErr)
		}
		return out, fp, nil
	case <-time.After(timeout):
		_ = sess.Close()
		return buf.String(), fp, fmt.Errorf("更新超时（%s）", timeout)
	}
}
