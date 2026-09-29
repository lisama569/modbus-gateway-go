package mapping

import "fmt"

// UncertainWriter 表示"写请求可能已生效但最终状态无法确认"的错误语义。
// 适配层返回的错误若实现该接口,写批次会把对应点位归类为 uncertain 而非确定失败。
type UncertainWriter interface {
	error
	UncertainOutcome() bool
}

// AttemptCarrier 由底层写入错误可选实现:携带该片段的物理写入尝试次数。
type AttemptCarrier interface {
	WriteAttempts() int
}

// classifyWriteErr 把底层写入错误归一化为单项错误:
// 实现 UncertainWriter -> uncertain(值可能已生效,未盲判成败);
// 其余(含对端明确拒绝/非法参数) -> transport(确定失败)。
func classifyWriteErr(p Point, err error) error {
	name := "point " + p.Name + ": "
	var u UncertainWriter
	if asUncertain(err, &u) && u.UncertainOutcome() {
		return &MapError{Kind: "uncertain", Msg: name + err.Error()}
	}
	return &MapError{Kind: "transport", Msg: name + err.Error()}
}

func asUncertain(err error, target *UncertainWriter) bool {
	for err != nil {
		if u, ok := err.(UncertainWriter); ok {
			*target = u
			return true
		}
		u2, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u2.Unwrap()
	}
	return false
}

func goTypeName(v any) string {
	if v == nil {
		return "nil"
	}
	return fmt.Sprintf("%T", v)
}
