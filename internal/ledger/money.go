package ledger

import (
	"fmt"
	"strconv"
	"strings"
)

// maxAbsAmount — предохранитель от переполнения int64 при сложении.
// 1e15 минимальных единиц при scale=6 — это 1 млрд единиц валюты на одну
// проводку. В проде на Postgres стоит numeric(38,18), и такой границы нет.
const maxAbsAmount int64 = 1_000_000_000_000_000

// addInt64 складывает с проверкой переполнения. Go при переполнении
// молча заворачивает значение, а в деньгах это недопустимо.
func addInt64(a, b int64) (int64, bool) {
	s := a + b
	if (b > 0 && s < a) || (b < 0 && s > a) {
		return 0, false
	}
	return s, true
}

// FormatAmount печатает сумму в минимальных единицах как десятичную дробь.
// Только для логов и демо: арифметика всегда идёт в int64.
func FormatAmount(amount int64, scale int32) string {
	if scale < 0 {
		scale = 0
	}

	var u uint64
	neg := amount < 0
	if neg {
		// -(MinInt64) переполняется, поэтому сначала сдвигаем на единицу.
		u = uint64(-(amount + 1)) + 1
	} else {
		u = uint64(amount)
	}

	digits := strconv.FormatUint(u, 10)
	if scale > 0 {
		for len(digits) <= int(scale) {
			digits = "0" + digits
		}
		cut := len(digits) - int(scale)
		digits = digits[:cut] + "." + digits[cut:]
	}
	if neg {
		digits = "-" + digits
	}
	return digits
}

// FormatMoney добавляет код валюты: "250.000000 USDT".
func FormatMoney(amount int64, scale int32, currency string) string {
	return fmt.Sprintf("%s %s", FormatAmount(amount, scale), strings.TrimSpace(currency))
}
