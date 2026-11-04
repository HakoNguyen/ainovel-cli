package web

// loopGuard 盯着流式输出的尾部。模型偶尔会陷入短片段的无限复读（实测
// deepseek-v4.1-flash：`"the "the "the …` 连续数分钟），引擎没有退出条件，
// 会一路烧到 max_tokens。检测到就由网页端暂停引擎并提示换模型。
//
// 判据：最近 loopWindow 字节完全由一个不超过 loopMaxPeriod 字节的单元周期重复构成。
// 正常行文、JSON 参数、Markdown 都不会出现上千字节逐字节的短周期。
type loopGuard struct {
	tail    []byte
	tripped bool
}

const (
	loopWindow    = 1200
	loopMaxPeriod = 40
)

func (g *loopGuard) reset() {
	g.tail = g.tail[:0]
	g.tripped = false
}

// feed 追加一段输出；返回 true 表示刚刚判定为复读，sample 是重复单元的示例。
// 同一轮只触发一次，直到 reset。
func (g *loopGuard) feed(delta string) (tripped bool, sample string) {
	g.tail = append(g.tail, delta...)
	if len(g.tail) > 2*loopWindow {
		g.tail = append(g.tail[:0], g.tail[len(g.tail)-loopWindow:]...)
	}
	if g.tripped || len(g.tail) < loopWindow {
		return false, ""
	}
	w := g.tail[len(g.tail)-loopWindow:]
	for p := 1; p <= loopMaxPeriod; p++ {
		periodic := true
		for i := p; i < len(w); i++ {
			if w[i] != w[i-p] {
				periodic = false
				break
			}
		}
		if periodic {
			g.tripped = true
			n := p * 3
			if n > 60 {
				n = 60
			}
			return true, string(w[len(w)-n:])
		}
	}
	return false, ""
}
