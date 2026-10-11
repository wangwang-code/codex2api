package security

import (
	"fmt"
	"net"
	"strings"
	"sync/atomic"
)

// ==================== 全局强制 hosts 覆盖 ====================
//
// 背景：某些代理/网络环境下，域名解析会落在错误的远端（例如 socks5h 由代理侧
// 解析、或代理链路里存在错误的 DNS），导致连接被指到错误的目标。本模块提供一份
// 进程级的「强制 hosts」映射，把所有出站拨号里的域名替换成显式 IP，使：
//
//   - http:// / https:// 代理：CONNECT 的目标地址用 IP，不再由代理解析；
//   - socks5:// / socks5h:// 代理：SOCKS5 请求里下发 IP（ATYP=IPv4/IPv6），
//     而不是域名，绕开代理侧的远端 DNS；
//   - 直连：直接连到覆盖 IP；
//   - 代理服务器本身的域名（proxy.example.com:1080）同样被强制成 IP。
//
// 只改写「拨号地址」，不改写 TLS SNI / Host 头：握手仍然按原域名进行，证书校验
// 与虚拟主机路由不受影响。
//
// 配置来自环境变量 CODEX_HOSTS_OVERRIDE，支持两种写法（可混用，换行/逗号/分号分隔）：
//
//	CODEX_HOSTS_OVERRIDE="api.openai.com=1.2.3.4,chatgpt.com=5.6.7.8"
//	CODEX_HOSTS_OVERRIDE="api.openai.com 1.2.3.4
//	chatgpt.com 5.6.7.8   # hosts 文件风格，支持 # 注释"
//
// 未列出的域名保持原样，走系统解析。

// EnvHostsOverride 是强制 hosts 覆盖的环境变量名。
const EnvHostsOverride = "CODEX_HOSTS_OVERRIDE"

// HostsOverride 是一份 hostname -> IP 字面量的强制映射。
type HostsOverride struct {
	entries map[string]string
}

// ParseHostsOverride 解析强制 hosts 覆盖配置。
// 空输入返回一个未启用（Enabled()==false）的空映射，不报错。
// 任何非法条目都会返回错误，便于在启动阶段 fail-fast。
func ParseHostsOverride(raw string) (*HostsOverride, error) {
	h := &HostsOverride{entries: make(map[string]string)}
	for _, field := range splitHostsOverrideFields(raw) {
		line := stripHostsComment(field)
		if line == "" {
			continue
		}
		host, ip, err := splitHostsEntry(line)
		if err != nil {
			return nil, err
		}
		h.entries[host] = ip
	}
	return h, nil
}

// Enabled 报告该映射是否至少有一条有效覆盖。
func (h *HostsOverride) Enabled() bool {
	return h != nil && len(h.entries) > 0
}

// Len 返回覆盖条数。
func (h *HostsOverride) Len() int {
	if h == nil {
		return 0
	}
	return len(h.entries)
}

// Lookup 返回 host 对应的强制 IP。host 大小写不敏感，末尾点会被忽略。
func (h *HostsOverride) Lookup(host string) (string, bool) {
	if !h.Enabled() {
		return "", false
	}
	key := normalizeOverrideHost(host)
	if key == "" {
		return "", false
	}
	ip, ok := h.entries[key]
	return ip, ok
}

// Entries 返回映射副本（host -> ip），供日志/展示使用。
func (h *HostsOverride) Entries() map[string]string {
	out := make(map[string]string, h.Len())
	if h == nil {
		return out
	}
	for k, v := range h.entries {
		out[k] = v
	}
	return out
}

// RewriteHostPort 把 "host:port" 里的 host 换成覆盖 IP；没有覆盖或本来就是
// IP 字面量时原样返回。port 可为空（此时只返回 IP）。
func (h *HostsOverride) RewriteHostPort(addr string) string {
	if !h.Enabled() || addr == "" {
		return addr
	}
	host, port := splitHostPortLoose(addr)
	if host == "" {
		return addr
	}
	ip, ok := h.Lookup(host)
	if !ok {
		return addr
	}
	if port == "" {
		return ip
	}
	return net.JoinHostPort(ip, port)
}

var globalHostsOverride atomic.Pointer[HostsOverride]

// SetHostsOverride 解析并安装全局强制 hosts 覆盖。空串表示清除（关闭该功能）。
func SetHostsOverride(raw string) error {
	parsed, err := ParseHostsOverride(raw)
	if err != nil {
		return err
	}
	if parsed.Enabled() {
		globalHostsOverride.Store(parsed)
	} else {
		globalHostsOverride.Store(nil)
	}
	return nil
}

// CurrentHostsOverride 返回当前生效的覆盖，未启用时返回 nil（其方法对 nil 安全）。
func CurrentHostsOverride() *HostsOverride {
	return globalHostsOverride.Load()
}

// HostsOverrideEnabled 报告当前是否配置了强制 hosts 覆盖。
func HostsOverrideEnabled() bool {
	return CurrentHostsOverride().Enabled()
}

// RewriteHostPort 按全局覆盖改写 "host:port"，未启用或未命中时原样返回。
func RewriteHostPort(addr string) string {
	return CurrentHostsOverride().RewriteHostPort(addr)
}

// RewriteHost 按全局覆盖查询 host 对应的强制 IP。
func RewriteHost(host string) (string, bool) {
	return CurrentHostsOverride().Lookup(host)
}

// ---- 解析辅助 ----

// splitHostsOverrideFields 按换行/逗号/分号切分条目（不切空格：hosts 文件风格
// 用空格分隔 host 与 ip）。
func splitHostsOverrideFields(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	return strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ';'
	})
}

// stripHostsComment 去掉条目里的 # 注释并 trim。
func stripHostsComment(line string) string {
	if idx := strings.IndexByte(line, '#'); idx >= 0 {
		line = line[:idx]
	}
	return strings.TrimSpace(line)
}

// splitHostsEntry 解析单条 "host=ip" 或 "host ip"。
func splitHostsEntry(line string) (host, ip string, err error) {
	if idx := strings.IndexByte(line, '='); idx >= 0 {
		host = strings.TrimSpace(line[:idx])
		ip = strings.TrimSpace(line[idx+1:])
	} else {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return "", "", fmt.Errorf("非法的 hosts 覆盖条目 %q（应为 host=ip 或 \"host ip\"）", line)
		}
		host, ip = fields[0], fields[1]
	}

	key := normalizeOverrideHost(host)
	if key == "" {
		return "", "", fmt.Errorf("hosts 覆盖条目缺少域名: %q", line)
	}
	if strings.ContainsAny(key, "/:[]") {
		return "", "", fmt.Errorf("hosts 覆盖的域名非法（不要带端口/schema）: %q", host)
	}
	if net.ParseIP(ip) == nil {
		return "", "", fmt.Errorf("hosts 覆盖 %q 的目标 %q 不是合法 IP", host, ip)
	}
	return key, ip, nil
}

// normalizeOverrideHost 统一域名键：小写、去首尾空白、去末尾点、去 IPv6 方括号。
func normalizeOverrideHost(host string) string {
	host = strings.TrimSpace(host)
	host = strings.Trim(host, "[]")
	host = strings.TrimSuffix(host, ".")
	return strings.ToLower(host)
}

// splitHostPortLoose 宽松拆分 "host:port"：没有端口时把整体当 host。
func splitHostPortLoose(addr string) (host, port string) {
	if h, p, err := net.SplitHostPort(addr); err == nil {
		return h, p
	}
	return strings.Trim(addr, "[]"), ""
}
