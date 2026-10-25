package service

import (
	"bufio"
	"io"
	"time"
)

// Anthropic 通常在 message_stop 前通过 message_delta 报告 usage，但兼容上游
// 可能在 stop 后补发 usage。固定尾窗口保留这部分容错，同时避免恢复无限等 EOF
// 的旧问题；窗口之外才到达的 usage 不保证被收集。
const gatewayForwardStreamTailGrace = 500 * time.Millisecond

// gatewayForwardStreamDrain 复用 native Anthropic 行泵，并接管响应 body 的关闭。
// terminal、闲置超时或读错退出时，关闭 body 并等待行泵结束，避免脱离客户端取消
// 的上游读协程继续阻塞。只有 handler 协程消费事件和操作下游 writer。
type gatewayForwardStreamDrain struct {
	pump         *anthropicNativeLinePump
	body         io.ReadCloser
	tailDeadline time.Time
	tailTimer    *time.Timer
}

func newGatewayForwardStreamDrain(scanner *bufio.Scanner, body io.ReadCloser, interval time.Duration) *gatewayForwardStreamDrain {
	return &gatewayForwardStreamDrain{
		pump: newAnthropicNativeLinePump(scanner, interval),
		body: body,
	}
}

// startTerminalTail 首次收到 message_stop 时启动固定尾窗口；后续 terminal、
// keepalive 或 usage 都不能延长总期限。
func (d *gatewayForwardStreamDrain) startTerminalTail() {
	if d.tailTimer != nil {
		return
	}
	d.tailDeadline = time.Now().Add(gatewayForwardStreamTailGrace)
	d.tailTimer = time.NewTimer(gatewayForwardStreamTailGrace)
}

func (d *gatewayForwardStreamDrain) terminalSeen() bool {
	return d.tailTimer != nil
}

// nextOrClientGone 在等待上游行时同时观察客户端断开。terminal 固定期限、
// 逐行 idle 计时器和已缓冲事件均沿用同一套优先级；clientGone 仅影响等待，
// 不阻止继续排水和合并 usage。
func (d *gatewayForwardStreamDrain) nextOrClientGone(clientGone <-chan struct{}) (string, error) {
	var tailCh, idleCh <-chan time.Time
	if d.tailTimer != nil {
		// 持续到达的事件不能因 select 总选中队列而拖长尾窗口。
		if !time.Now().Before(d.tailDeadline) {
			return "", io.EOF
		}
		tailCh = d.tailTimer.C
	}
	if d.pump.timer != nil {
		idleCh = d.pump.timer.C
	}

	// 已缓冲事件优先于 idle 和连接取消；terminal deadline 已在上面优先判定。
	select {
	case ev, ok := <-d.pump.events:
		if !ok {
			return "", io.EOF
		}
		if d.tailTimer != nil && !time.Now().Before(d.tailDeadline) {
			return "", io.EOF
		}
		d.pump.resetTimer()
		return ev.line, ev.err
	default:
	}

	select {
	case ev, ok := <-d.pump.events:
		if !ok {
			return "", io.EOF
		}
		// 如果事件与 terminal 定时器同时就绪，固定期限仍然优先。
		if d.tailTimer != nil && !time.Now().Before(d.tailDeadline) {
			return "", io.EOF
		}
		d.pump.resetTimer()
		return ev.line, ev.err
	case <-tailCh:
		return "", io.EOF
	case <-idleCh:
		// 与 native pump 一致：计时器到期时若已有缓冲行，优先交付该行。
		select {
		case ev, ok := <-d.pump.events:
			if !ok {
				return "", io.EOF
			}
			d.pump.resetTimer()
			return ev.line, ev.err
		default:
			return "", errAnthropicNativeStreamIdle
		}
	case <-clientGone:
		return "", errAnthropicNativeClientGone
	}
}

func (d *gatewayForwardStreamDrain) stop() {
	d.pump.stop()
	if d.tailTimer != nil {
		d.tailTimer.Stop()
	}
	if d.body != nil {
		_ = d.body.Close() // 先解除正在进行的 Scan 阻塞，再等行泵退出。
	}
	for range d.pump.events {
	}
}
