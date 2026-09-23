package proxy

import (
	"log"
	"os"
	"strings"
)

// 本文件放网关自有的环境变量解析工具。伪装思考与上游错误改写都要用同一套语义，
// 避免各自实现导致「同一个 \n 在 A 开关里生效、在 B 开关里失效」这类不一致。

// boolFromEnv 解析布尔型环境变量；无法识别时沿用默认值并记录日志。
func boolFromEnv(key string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		log.Printf("[Config] %s=%q 非法，沿用默认 %t", key, raw, fallback)
		return fallback
	}
}

// decodeEnvEscapes 把 `\n` / `\r` / `\t` / `\\` / `\"` 等转义还原成真实字符。
// .env 本身不做转义（只有双引号值会被 godotenv 预处理），所以从 YAML 配置平移过来
// 的文案必须靠这里补齐；无法识别的转义原样保留，避免误伤正常的反斜杠。
func decodeEnvEscapes(value string) string {
	if !strings.Contains(value, `\`) {
		return value
	}
	var out strings.Builder
	out.Grow(len(value))
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' || i+1 >= len(value) {
			out.WriteByte(value[i])
			continue
		}
		i++
		switch value[i] {
		case 'n':
			out.WriteByte('\n')
		case 'r':
			out.WriteByte('\r')
		case 't':
			out.WriteByte('\t')
		case '\\':
			out.WriteByte('\\')
		case '"':
			out.WriteByte('"')
		case '\'':
			out.WriteByte('\'')
		case '0':
			out.WriteByte(0)
		default:
			out.WriteByte('\\')
			out.WriteByte(value[i])
		}
	}
	return out.String()
}
