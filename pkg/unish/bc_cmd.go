package unish

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/interp"
)

var (
	bigZero = big.NewInt(0)
	bigOne  = big.NewInt(1)
	bigTwo  = big.NewInt(2)
	bigFive = big.NewInt(5)
	bigTen  = big.NewInt(10)
)

// pow10Cache caches 10^n up to modest n to avoid repeated allocations.
var pow10Small = func() []*big.Int {
	arr := make([]*big.Int, 129)
	arr[0] = big.NewInt(1)
	for i := 1; i < len(arr); i++ {
		arr[i] = new(big.Int).Mul(arr[i-1], bigTen)
	}
	return arr
}()

func bcPow10(n int) *big.Int {
	if n <= 0 {
		return new(big.Int).SetInt64(1)
	}
	if n < len(pow10Small) {
		return new(big.Int).Set(pow10Small[n])
	}
	// Square-and-multiply by hand: big.Int.Exp would drag the modular
	// exponentiation (Montgomery/windowed/GCD) machinery into the binary.
	res := big.NewInt(1)
	base := big.NewInt(10)
	for e := n; e > 0; e >>= 1 {
		if e&1 == 1 {
			res.Mul(res, base)
		}
		if e > 1 {
			base.Mul(base, base)
		}
	}
	return res
}

// bcNum represents an exact fixed-point decimal: mant * 10^(-sc), sc >= 0.
type bcNum struct {
	mant *big.Int
	sc   int
}

func bcZero() bcNum {
	return bcNum{mant: new(big.Int), sc: 0}
}

func bcFromInt64(v int64) bcNum {
	return bcNum{mant: big.NewInt(v), sc: 0}
}

func (n bcNum) clone() bcNum {
	if n.mant == nil {
		return bcZero()
	}
	return bcNum{mant: new(big.Int).Set(n.mant), sc: n.sc}
}

func (n bcNum) isZero() bool {
	return n.mant == nil || n.mant.Sign() == 0
}

func (n bcNum) truncInt64() (int64, bool) {
	if n.mant == nil {
		return 0, true
	}
	z := new(big.Int).Set(n.mant)
	if n.sc > 0 {
		z.Quo(z, bcPow10(n.sc))
	}
	if !z.IsInt64() {
		return 0, false
	}
	return z.Int64(), true
}

func bcAlign(a, b bcNum) (*big.Int, *big.Int, int) {
	am := new(big.Int)
	if a.mant != nil {
		am.Set(a.mant)
	}
	bm := new(big.Int)
	if b.mant != nil {
		bm.Set(b.mant)
	}
	if a.sc == b.sc {
		return am, bm, a.sc
	}
	if a.sc < b.sc {
		am.Mul(am, bcPow10(b.sc-a.sc))
		return am, bm, b.sc
	}
	bm.Mul(bm, bcPow10(a.sc-b.sc))
	return am, bm, a.sc
}

func bcAdd(a, b bcNum) bcNum {
	am, bm, sc := bcAlign(a, b)
	am.Add(am, bm)
	return bcNum{mant: am, sc: sc}
}

func bcSub(a, b bcNum) bcNum {
	am, bm, sc := bcAlign(a, b)
	am.Sub(am, bm)
	return bcNum{mant: am, sc: sc}
}

func bcCmp(a, b bcNum) int {
	am, bm, _ := bcAlign(a, b)
	return am.Cmp(bm)
}

func bcNeg(a bcNum) bcNum {
	c := a.clone()
	c.mant.Neg(c.mant)
	return c
}

func bcMul(a, b bcNum, globalScale int) bcNum {
	prod := new(big.Int)
	if a.mant != nil && b.mant != nil {
		prod.Mul(a.mant, b.mant)
	}
	sumSc := a.sc + b.sc
	resSc := min(sumSc, max(globalScale, max(a.sc, b.sc)))
	if sumSc > resSc {
		prod.Quo(prod, bcPow10(sumSc-resSc))
	}
	return bcNum{mant: prod, sc: resSc}
}

func bcDiv(a, b bcNum, globalScale int) (bcNum, error) {
	if b.isZero() {
		return bcZero(), errors.New("Runtime error: divide by zero")
	}
	num := new(big.Int)
	if a.mant != nil {
		num.Set(a.mant)
	}
	den := new(big.Int).Set(b.mant)
	shift := globalScale + b.sc - a.sc
	if shift > 0 {
		num.Mul(num, bcPow10(shift))
	} else if shift < 0 {
		den.Mul(den, bcPow10(-shift))
	}
	num.Quo(num, den)
	return bcNum{mant: num, sc: globalScale}, nil
}

func bcMod(a, b bcNum, globalScale int) (bcNum, error) {
	if b.isZero() {
		return bcZero(), errors.New("Runtime error: modulo by zero")
	}
	q, err := bcDiv(a, b, globalScale)
	if err != nil {
		return bcZero(), err
	}
	// Exact q * b at scale (globalScale + b.sc) without truncation, then a - (q * b)
	qb := bcNum{
		mant: new(big.Int).Mul(q.mant, b.mant),
		sc:   q.sc + b.sc,
	}
	return bcSub(a, qb), nil
}

func bcPow(ctx context.Context, a, b bcNum, globalScale int) (bcNum, error) {
	expVal, ok := b.truncInt64()
	if !ok || expVal > 1_000_000 || expVal < -1_000_000 {
		return bcZero(), errors.New("Runtime error: exponent too large")
	}
	if expVal == 0 {
		return bcFromInt64(1), nil
	}
	neg := expVal < 0
	if neg {
		expVal = -expVal
	}

	// Compute a^expVal
	rScale := a.sc * int(expVal)
	targetSc := rScale
	if !neg {
		targetSc = min(rScale, max(globalScale, a.sc))
	} else {
		targetSc = globalScale
	}

	// Binary exponentiation on mantissas; truncate intermediate fractional digits safely if huge.
	res := bcFromInt64(1)
	base := a.clone()
	workScale := max(targetSc+16, max(globalScale, a.sc))
	e := expVal
	for e > 0 {
		if ctx.Err() != nil {
			return bcZero(), ctx.Err()
		}
		if e&1 == 1 {
			res = bcMulExactCap(res, base, workScale)
		}
		e >>= 1
		if e > 0 {
			base = bcMulExactCap(base, base, workScale)
		}
	}

	if neg {
		return bcDiv(bcFromInt64(1), res, globalScale)
	}
	if res.sc > targetSc {
		res.mant.Quo(res.mant, bcPow10(res.sc-targetSc))
		res.sc = targetSc
	}
	return res, nil
}

func bcMulExactCap(a, b bcNum, maxSc int) bcNum {
	p := new(big.Int).Mul(a.mant, b.mant)
	sc := a.sc + b.sc
	if a.sc > 0 || b.sc > 0 {
		if sc > maxSc {
			p.Quo(p, bcPow10(sc-maxSc))
			sc = maxSc
		}
	}
	return bcNum{mant: p, sc: sc}
}

func bcSqrt(a bcNum, globalScale int) (bcNum, error) {
	if a.mant != nil && a.mant.Sign() < 0 {
		return bcZero(), errors.New("Runtime error: square root of negative number")
	}
	rSc := max(globalScale, a.sc)
	if a.isZero() {
		return bcNum{mant: new(big.Int), sc: rSc}, nil
	}
	scaled := new(big.Int).Set(a.mant)
	shift := 2*rSc - a.sc
	if shift > 0 {
		scaled.Mul(scaled, bcPow10(shift))
	} else if shift < 0 {
		scaled.Quo(scaled, bcPow10(-shift))
	}
	root := new(big.Int).Sqrt(scaled)
	return bcNum{mant: root, sc: rSc}, nil
}

// bcDecString renders |x| in base 10. It peels 18-digit chunks with
// single-word divisions instead of calling big.Int.String, whose
// divide-and-conquer conversion links in modular exponentiation and GCD
// code (~20 KB) that bc never otherwise needs.
func bcDecString(x *big.Int) string {
	if x.Sign() == 0 {
		return "0"
	}
	q := new(big.Int).Abs(x)
	chunk := big.NewInt(1_000_000_000_000_000_000)
	r := new(big.Int)
	var parts []uint64
	for q.Sign() > 0 {
		q.QuoRem(q, chunk, r)
		parts = append(parts, r.Uint64())
	}
	b := strconv.AppendUint(make([]byte, 0, len(parts)*18), parts[len(parts)-1], 10)
	var tmp [20]byte
	for i := len(parts) - 2; i >= 0; i-- {
		d := strconv.AppendUint(tmp[:0], parts[i], 10)
		for pad := 18 - len(d); pad > 0; pad-- {
			b = append(b, '0')
		}
		b = append(b, d...)
	}
	return string(b)
}

func bcLength(a bcNum) bcNum {
	if a.isZero() {
		if a.sc == 0 {
			return bcFromInt64(1)
		}
		return bcFromInt64(int64(a.sc))
	}
	absStr := bcDecString(a.mant)
	d := len(absStr)
	if d < a.sc {
		d = a.sc
	}
	return bcFromInt64(int64(d))
}

func bcParseDecUint(s string) *big.Int {
	z := new(big.Int)
	var acc, mul int64 = 0, 1
	for i := 0; i < len(s); i++ {
		d := s[i]
		if d < '0' || d > '9' {
			d = '9'
		}
		acc = acc*10 + int64(d-'0')
		mul *= 10
		if mul == 1_000_000_000_000_000_000 {
			z.Mul(z, big.NewInt(mul))
			z.Add(z, big.NewInt(acc))
			acc, mul = 0, 1
		}
	}
	if mul > 1 {
		z.Mul(z, big.NewInt(mul))
		z.Add(z, big.NewInt(acc))
	}
	return z
}

// parseBcLiteral parses a raw bc number token under the current ibase.
func parseBcLiteral(raw string, ibase int) bcNum {
	// POSIX/GNU bc rule: single character 0-9 / A-Z without '.' always has its intrinsic hex/alnum value!
	if len(raw) == 1 && raw[0] != '.' {
		v := bcDigitVal(raw[0])
		return bcFromInt64(int64(v))
	}

	if ibase == 10 {
		dot := strings.IndexByte(raw, '.')
		if dot < 0 {
			return bcNum{mant: bcParseDecUint(raw), sc: 0}
		}
		sc := len(raw) - dot - 1
		return bcNum{mant: bcParseDecUint(raw[:dot] + raw[dot+1:]), sc: sc}
	}

	ibBig := big.NewInt(int64(ibase))
	dot := strings.IndexByte(raw, '.')
	intPartStr := raw
	fracPartStr := ""
	if dot >= 0 {
		intPartStr = raw[:dot]
		fracPartStr = raw[dot+1:]
	}

	intVal := new(big.Int)
	for i := 0; i < len(intPartStr); i++ {
		d := bcDigitVal(intPartStr[i])
		if d >= ibase {
			d = ibase - 1
		}
		intVal.Mul(intVal, ibBig)
		intVal.Add(intVal, big.NewInt(int64(d)))
	}

	if len(fracPartStr) == 0 {
		return bcNum{mant: intVal, sc: 0}
	}

	fracNum := new(big.Int)
	fracDen := big.NewInt(1)
	for i := 0; i < len(fracPartStr); i++ {
		d := bcDigitVal(fracPartStr[i])
		if d >= ibase {
			d = ibase - 1
		}
		fracNum.Mul(fracNum, ibBig)
		fracNum.Add(fracNum, big.NewInt(int64(d)))
		fracDen.Mul(fracDen, ibBig)
	}

	// Scale in POSIX bc for non-10 ibase fractional literals: number of fractional digits in input!
	sc := len(fracPartStr)
	tenSc := bcPow10(sc)
	fracScaled := new(big.Int).Mul(fracNum, tenSc)
	fracScaled.Quo(fracScaled, fracDen)

	total := new(big.Int).Mul(intVal, tenSc)
	total.Add(total, fracScaled)
	return bcNum{mant: total, sc: sc}
}

func bcDigitVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'A' && c <= 'Z':
		return int(c-'A') + 10
	default:
		return 0
	}
}

func formatBcNum(n bcNum, obase int) string {
	if n.isZero() {
		return "0"
	}
	neg := n.mant.Sign() < 0
	absMant := new(big.Int).Abs(n.mant)

	if obase == 10 {
		var sb strings.Builder
		if neg {
			sb.WriteByte('-')
		}
		s := bcDecString(absMant)
		if n.sc == 0 {
			sb.WriteString(s)
			return sb.String()
		}
		if len(s) <= n.sc {
			sb.WriteByte('.')
			for k := 0; k < n.sc-len(s); k++ {
				sb.WriteByte('0')
			}
			sb.WriteString(s)
			return sb.String()
		}
		split := len(s) - n.sc
		sb.WriteString(s[:split])
		sb.WriteByte('.')
		sb.WriteString(s[split:])
		return sb.String()
	}

	// Non-10 obase (>= 2)
	tenSc := bcPow10(n.sc)
	intPart := new(big.Int).Quo(absMant, tenSc)
	fracPart := new(big.Int).Rem(absMant, tenSc)
	obBig := big.NewInt(int64(obase))

	if obase <= 16 {
		const hexChars = "0123456789ABCDEF"
		var sb strings.Builder
		if neg {
			sb.WriteByte('-')
		}
		if intPart.Sign() > 0 || n.sc == 0 {
			if intPart.Sign() == 0 {
				sb.WriteByte('0')
			} else {
				var digits []byte
				tmp := new(big.Int).Set(intPart)
				rem := new(big.Int)
				for tmp.Sign() > 0 {
					tmp.QuoRem(tmp, obBig, rem)
					digits = append(digits, hexChars[rem.Int64()])
				}
				for i := len(digits) - 1; i >= 0; i-- {
					sb.WriteByte(digits[i])
				}
			}
		}
		if n.sc > 0 {
			sb.WriteByte('.')
			// Number of base-obase fractional digits sufficient to represent 10^(-sc)
			outDigits := n.sc
			if obase < 10 {
				// ceil(sc * ln(10)/ln(obase)): for base 2, ~4*sc
				tmpPow := big.NewInt(1)
				outDigits = 0
				for tmpPow.Cmp(tenSc) < 0 {
					tmpPow.Mul(tmpPow, obBig)
					outDigits++
				}
			}
			rem := new(big.Int).Set(fracPart)
			q := new(big.Int)
			for i := 0; i < outDigits; i++ {
				rem.Mul(rem, obBig)
				q.QuoRem(rem, tenSc, rem)
				sb.WriteByte(hexChars[q.Int64()])
			}
		}
		return sb.String()
	}

	// POSIX obase > 16: space-separated zero-padded decimal groups
	groupWidth := len(strconv.Itoa(obase - 1))
	var sb strings.Builder
	if neg {
		sb.WriteByte('-')
	}
	var intGroups []int64
	if intPart.Sign() == 0 && n.sc == 0 {
		intGroups = []int64{0}
	} else {
		tmp := new(big.Int).Set(intPart)
		rem := new(big.Int)
		for tmp.Sign() > 0 {
			tmp.QuoRem(tmp, obBig, rem)
			intGroups = append(intGroups, rem.Int64())
		}
	}
	for i := len(intGroups) - 1; i >= 0; i-- {
		fmt.Fprintf(&sb, " %0*d", groupWidth, intGroups[i])
	}
	if n.sc > 0 {
		sb.WriteString(" .")
		rem := new(big.Int).Set(fracPart)
		q := new(big.Int)
		for i := 0; i < n.sc; i++ {
			rem.Mul(rem, obBig)
			q.QuoRem(rem, tenSc, rem)
			fmt.Fprintf(&sb, " %0*d", groupWidth, q.Int64())
		}
	}
	return sb.String()
}

// ---- Arbitrary-Precision Math Library (bc -l: s, c, a, l, e, j) ----

// Fixed-point internal helpers representing x as V * 10^(-P).
func bcToFP(x bcNum, P int) *big.Int {
	v := new(big.Int)
	if x.mant != nil {
		v.Set(x.mant)
	}
	if P > x.sc {
		v.Mul(v, bcPow10(P-x.sc))
	} else if P < x.sc {
		v.Quo(v, bcPow10(x.sc-P))
	}
	return v
}

func fpToBc(v *big.Int, P, targetScale int) bcNum {
	res := new(big.Int).Set(v)
	if P > targetScale {
		res.Quo(res, bcPow10(P-targetScale))
	} else if P < targetScale {
		res.Mul(res, bcPow10(targetScale-P))
	}
	return bcNum{mant: res, sc: targetScale}
}

// fpAtanh2Inv computes 2 * sum_{k=0..inf} (1/m)^(2k+1) / (2k+1) = ln((m+1)/(m-1)) at precision P.
func fpAtanh2Inv(ctx context.Context, m int64, P int) *big.Int {
	scalePow := bcPow10(P)
	mBig := big.NewInt(m)
	m2Big := big.NewInt(m * m)
	term := new(big.Int).Quo(new(big.Int).Mul(bigTwo, scalePow), mBig)
	sum := new(big.Int).Set(term)
	tDiv := new(big.Int)
	for k := int64(1); ; k++ {
		if k&63 == 0 && ctx.Err() != nil {
			return sum
		}
		term.Quo(term, m2Big)
		if term.Sign() == 0 {
			break
		}
		tDiv.Quo(term, big.NewInt(2*k+1))
		if tDiv.Sign() == 0 {
			break
		}
		sum.Add(sum, tDiv)
	}
	return sum
}

func fpLn2(ctx context.Context, P int) *big.Int {
	// ln(2) = ln((3+1)/(3-1)) -> m = 3
	return fpAtanh2Inv(ctx, 3, P)
}

func fpLn10(ctx context.Context, P int) *big.Int {
	// ln(10) = 3*ln(2) + ln(5/4) = 3*fpAtanh2Inv(3) + fpAtanh2Inv(9)
	ln2 := fpAtanh2Inv(ctx, 3, P+4)
	ln125 := fpAtanh2Inv(ctx, 9, P+4)
	sum := new(big.Int).Mul(ln2, big.NewInt(3))
	sum.Add(sum, ln125)
	return sum.Quo(sum, bcPow10(4))
}

func bcMathExp(ctx context.Context, x bcNum, targetScale int) (bcNum, error) {
	if x.isZero() {
		return bcNum{mant: bcPow10(targetScale), sc: targetScale}, nil
	}
	// Overflow guard
	if iv, ok := x.truncInt64(); !ok || iv > 50000 || iv < -50000 {
		return bcZero(), errors.New("Runtime error: e(x) exponent out of range")
	}

	neg := x.mant.Sign() < 0
	absX := x.clone()
	if neg {
		absX.mant.Neg(absX.mant)
	}

	// Extra guard digits proportional to integer magnitude of |x|
	intMag, _ := absX.truncInt64()
	extraDigits := int(intMag/2) + 24
	P := targetScale + extraDigits
	scalePow := bcPow10(P)

	// Reduce absX = k * ln(2) + r, so e^(absX) = 2^k * e^r with 0 <= r < ln(2)
	ln2 := fpLn2(ctx, P)
	xFP := bcToFP(absX, P)
	kBig, rFP := new(big.Int).QuoRem(xFP, ln2, new(big.Int))
	k := kBig.Int64()

	// Halve r 6 times so r/64 < 0.011
	const halvings = 6
	rFP.Rsh(rFP, halvings)

	// Taylor series sum_{n=0..inf} u^n / n!
	sum := new(big.Int).Set(scalePow)
	term := new(big.Int).Set(scalePow)
	for n := int64(1); ; n++ {
		if n&63 == 0 && ctx.Err() != nil {
			return bcZero(), ctx.Err()
		}
		term.Mul(term, rFP)
		term.Quo(term, scalePow)
		term.Quo(term, big.NewInt(n))
		if term.Sign() == 0 {
			break
		}
		sum.Add(sum, term)
	}

	// Square `halvings` times
	for i := 0; i < halvings; i++ {
		sum.Mul(sum, sum)
		sum.Quo(sum, scalePow)
	}

	// Multiply by 2^k
	if k > 0 {
		sum.Lsh(sum, uint(k))
	}

	if neg {
		// 1 / e^(|x|): (10^(2P)) / sum gives fixed-point at precision P
		num := bcPow10(2 * P)
		sum.Quo(num, sum)
	}
	return fpToBc(sum, P, targetScale), nil
}

func bcMathLn(ctx context.Context, x bcNum, targetScale int) (bcNum, error) {
	if x.isZero() || x.mant.Sign() < 0 {
		return bcZero(), errors.New("Runtime error: l(x) argument <= 0")
	}
	// Check exact 1
	if bcCmp(x, bcFromInt64(1)) == 0 {
		return bcNum{mant: new(big.Int), sc: targetScale}, nil
	}

	P := targetScale + 24
	scalePow := bcPow10(P)

	// Decompose x = u0 * 10^pow10Shift with u0 in [0.1, 1)
	digits := len(bcDecString(x.mant))
	pow10Shift := digits - x.sc
	// u0 = x.mant * 10^(-digits) -> in fixed-point P: x.mant * 10^(P - digits)
	u := new(big.Int).Set(x.mant)
	if P >= digits {
		u.Mul(u, bcPow10(P-digits))
	} else {
		u.Quo(u, bcPow10(digits-P))
	}

	// Take 6 square roots: u <- sqrt(u * 10^P) so u in [0.964, 1]
	const roots = 6
	for i := 0; i < roots; i++ {
		u.Mul(u, scalePow)
		u.Sqrt(u)
	}

	// ln(u) = 2 * sum_{k=0..inf} z^(2k+1) / (2k+1), where z = (u - 1) / (u + 1)
	uMinus1 := new(big.Int).Sub(u, scalePow)
	uPlus1 := new(big.Int).Add(u, scalePow)
	z := new(big.Int).Mul(uMinus1, scalePow)
	z.Quo(z, uPlus1)

	z2 := new(big.Int).Mul(z, z)
	z2.Quo(z2, scalePow)

	term := new(big.Int).Set(z)
	sum := new(big.Int).Set(z)
	tDiv := new(big.Int)
	for k := int64(1); ; k++ {
		if k&63 == 0 && ctx.Err() != nil {
			return bcZero(), ctx.Err()
		}
		term.Mul(term, z2)
		term.Quo(term, scalePow)
		if term.Sign() == 0 {
			break
		}
		tDiv.Quo(term, big.NewInt(2*k+1))
		if tDiv.Sign() == 0 {
			break
		}
		sum.Add(sum, tDiv)
	}
	// Multiply by 2 * 2^roots = 128
	sum.Lsh(sum, roots+1)

	if pow10Shift != 0 {
		ln10 := fpLn10(ctx, P)
		sum.Add(sum, new(big.Int).Mul(ln10, big.NewInt(int64(pow10Shift))))
	}
	return fpToBc(sum, P, targetScale), nil
}

// fpArctanInv computes arctan(1/m) at precision P for integer m >= 2.
func fpArctanInv(ctx context.Context, m int64, P int) *big.Int {
	scalePow := bcPow10(P)
	mBig := big.NewInt(m)
	m2Big := big.NewInt(m * m)
	term := new(big.Int).Quo(scalePow, mBig)
	sum := new(big.Int).Set(term)
	tDiv := new(big.Int)
	for k := int64(1); ; k++ {
		if k&63 == 0 && ctx.Err() != nil {
			return sum
		}
		term.Quo(term, m2Big)
		if term.Sign() == 0 {
			break
		}
		tDiv.Quo(term, big.NewInt(2*k+1))
		if tDiv.Sign() == 0 {
			break
		}
		if k&1 == 1 {
			sum.Sub(sum, tDiv)
		} else {
			sum.Add(sum, tDiv)
		}
	}
	return sum
}

func fpPi(ctx context.Context, P int) *big.Int {
	// Machin's formula: pi = 16*arctan(1/5) - 4*arctan(1/239)
	a5 := fpArctanInv(ctx, 5, P+4)
	a239 := fpArctanInv(ctx, 239, P+4)
	pi := new(big.Int).Mul(a5, big.NewInt(16))
	pi.Sub(pi, new(big.Int).Mul(a239, big.NewInt(4)))
	return pi.Quo(pi, bcPow10(4))
}

func bcMathAtan(ctx context.Context, x bcNum, targetScale int) (bcNum, error) {
	if x.isZero() {
		return bcNum{mant: new(big.Int), sc: targetScale}, nil
	}
	neg := x.mant.Sign() < 0
	absX := x.clone()
	if neg {
		absX.mant.Neg(absX.mant)
	}

	P := targetScale + 24
	scalePow := bcPow10(P)
	scalePow2 := bcPow10(2 * P)

	// If |x| > 1, use arctan(u) = pi/2 - arctan(1/u) so u <= 1 before halving!
	inverted := bcCmp(absX, bcFromInt64(1)) > 0
	var u *big.Int
	if inverted {
		// u = 1 / absX at precision P
		num := bcPow10(P + absX.sc)
		u = num.Quo(num, absX.mant)
	} else {
		u = bcToFP(absX, P)
	}

	// Argument reduction 8 times: u <- u / (1 + sqrt(1 + u^2)), so u <= tan(pi / 1024) < 0.0031
	const reductions = 8
	for i := 0; i < reductions; i++ {
		u2 := new(big.Int).Mul(u, u)
		rad := new(big.Int).Add(scalePow2, u2)
		sq := new(big.Int).Sqrt(rad) // at precision P!
		denom := new(big.Int).Add(scalePow, sq)
		num := new(big.Int).Mul(u, scalePow)
		u = num.Quo(num, denom)
	}

	// Taylor series sum_{k=0..inf} (-1)^k u^(2k+1) / (2k+1)
	u2 := new(big.Int).Mul(u, u)
	u2.Quo(u2, scalePow)
	term := new(big.Int).Set(u)
	sum := new(big.Int).Set(u)
	tDiv := new(big.Int)
	for k := int64(1); ; k++ {
		if k&63 == 0 && ctx.Err() != nil {
			return bcZero(), ctx.Err()
		}
		term.Mul(term, u2)
		term.Quo(term, scalePow)
		if term.Sign() == 0 {
			break
		}
		tDiv.Quo(term, big.NewInt(2*k+1))
		if tDiv.Sign() == 0 {
			break
		}
		if k&1 == 1 {
			sum.Sub(sum, tDiv)
		} else {
			sum.Add(sum, tDiv)
		}
	}
	sum.Lsh(sum, reductions)

	if inverted {
		piHalf := fpPi(ctx, P)
		piHalf.Rsh(piHalf, 1)
		sum.Sub(piHalf, sum)
	}
	if neg {
		sum.Neg(sum)
	}
	return fpToBc(sum, P, targetScale), nil
}

func bcMathSinCos(ctx context.Context, x bcNum, isCos bool, targetScale int) (bcNum, error) {
	if x.isZero() {
		if isCos {
			return bcNum{mant: bcPow10(targetScale), sc: targetScale}, nil
		}
		return bcNum{mant: new(big.Int), sc: targetScale}, nil
	}

	// Extra digits to preserve accuracy through x mod (2*pi) even for large x!
	intDigits := max(len(bcDecString(x.mant))-x.sc, 0)
	P := targetScale + intDigits + 24
	scalePow := bcPow10(P)

	pi := fpPi(ctx, P)
	twoPi := new(big.Int).Lsh(pi, 1)
	piHalf := new(big.Int).Rsh(pi, 1)

	xFP := bcToFP(x, P)
	// For cos(x), use sin(x + pi/2) so one unified high-precision reduction handles both!
	if isCos {
		xFP.Add(xFP, piHalf)
	}

	r := new(big.Int).Rem(xFP, twoPi)
	if r.Sign() < 0 {
		r.Add(r, twoPi)
	}
	// Now r in [0, 2*pi). Reduce to [-pi/2, pi/2] via symmetry of sin:
	negSign := false
	if r.Cmp(pi) > 0 {
		r.Sub(r, pi)
		negSign = true
	}
	if r.Cmp(piHalf) > 0 {
		r.Sub(pi, r)
	}

	// Taylor series for sin(r) on [0, pi/2]: sum_{k=0..inf} (-1)^k r^(2k+1) / (2k+1)!
	r2 := new(big.Int).Mul(r, r)
	r2.Quo(r2, scalePow)
	term := new(big.Int).Set(r)
	sum := new(big.Int).Set(r)
	for k := int64(1); ; k++ {
		if k&63 == 0 && ctx.Err() != nil {
			return bcZero(), ctx.Err()
		}
		term.Mul(term, r2)
		term.Quo(term, scalePow)
		term.Quo(term, big.NewInt((2*k)*(2*k+1)))
		if term.Sign() == 0 {
			break
		}
		if k&1 == 1 {
			sum.Sub(sum, term)
		} else {
			sum.Add(sum, term)
		}
	}
	if negSign {
		sum.Neg(sum)
	}
	return fpToBc(sum, P, targetScale), nil
}

func bcMathBesselJ(ctx context.Context, nArg, x bcNum, targetScale int) (bcNum, error) {
	n, ok := nArg.truncInt64()
	if !ok || n > 10000 || n < -10000 {
		return bcZero(), errors.New("Runtime error: j(n, x) order out of range")
	}
	negOrder := false
	if n < 0 {
		n = -n
		if n&1 == 1 {
			negOrder = true
		}
	}
	P := targetScale + 28
	scalePow := bcPow10(P)
	xFP := bcToFP(x, P)
	halfX := new(big.Int).Quo(xFP, bigTwo)

	// Initial term k=0: (x/2)^n / n!
	term := new(big.Int).Set(scalePow)
	for i := int64(1); i <= n; i++ {
		term.Mul(term, halfX)
		term.Quo(term, scalePow)
		term.Quo(term, big.NewInt(i))
	}
	sum := new(big.Int).Set(term)
	negHalfX2 := new(big.Int).Mul(halfX, halfX)
	negHalfX2.Quo(negHalfX2, scalePow)
	negHalfX2.Neg(negHalfX2)

	for k := int64(1); ; k++ {
		if k&63 == 0 && ctx.Err() != nil {
			return bcZero(), ctx.Err()
		}
		term.Mul(term, negHalfX2)
		term.Quo(term, scalePow)
		term.Quo(term, big.NewInt(k*(n+k)))
		if term.Sign() == 0 {
			break
		}
		sum.Add(sum, term)
	}
	if negOrder {
		sum.Neg(sum)
	}
	return fpToBc(sum, P, targetScale), nil
}

// ---- Lexer ----

type bcTokKind int

const (
	bcTokEOF bcTokKind = iota
	bcTokNL
	bcTokSemi
	bcTokComma
	bcTokLParen
	bcTokRParen
	bcTokLBrace
	bcTokRBrace
	bcTokLBrack
	bcTokRBrack
	bcTokNum
	bcTokStr
	bcTokIdent
	bcTokDot // '.' meaning `last`
	bcTokOp
)

type bcToken struct {
	kind bcTokKind
	val  string
	line int
}

func lexBc(src string) ([]bcToken, error) {
	// Strip line continuations `\\\r?\n` first (even inside multi-line numbers, like POSIX bc!).
	if strings.Contains(src, `\`) {
		var clean strings.Builder
		clean.Grow(len(src))
		for i := 0; i < len(src); i++ {
			if src[i] == '\\' {
				if i+1 < len(src) && src[i+1] == '\n' {
					i++
					continue
				}
				if i+2 < len(src) && src[i+1] == '\r' && src[i+2] == '\n' {
					i += 2
					continue
				}
			}
			clean.WriteByte(src[i])
		}
		src = clean.String()
	}

	var toks []bcToken
	line := 1
	i := 0
	for i < len(src) {
		c := src[i]
		if c == ' ' || c == '\t' || c == '\r' {
			i++
			continue
		}
		if c == '\n' {
			toks = append(toks, bcToken{kind: bcTokNL, val: "\n", line: line})
			line++
			i++
			continue
		}
		// Single-line '#' comment (GNU bc extension)
		if c == '#' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		}
		// Block comment /* ... */
		if c == '/' && i+1 < len(src) && src[i+1] == '*' {
			i += 2
			closed := false
			for i+1 < len(src) {
				if src[i] == '\n' {
					line++
				}
				if src[i] == '*' && src[i+1] == '/' {
					i += 2
					closed = true
					break
				}
				i++
			}
			if !closed {
				return nil, fmt.Errorf("syntax error at line %d: unterminated comment", line)
			}
			continue
		}
		// String literal "..."
		if c == '"' {
			startLine := line
			i++
			var sb strings.Builder
			closed := false
			for i < len(src) {
				if src[i] == '"' {
					i++
					closed = true
					break
				}
				if src[i] == '\n' {
					line++
				}
				sb.WriteByte(src[i])
				i++
			}
			if !closed {
				return nil, fmt.Errorf("syntax error at line %d: unterminated string", startLine)
			}
			toks = append(toks, bcToken{kind: bcTokStr, val: sb.String(), line: startLine})
			continue
		}

		// Number literal: starts with 0-9, A-Z, or '.' followed by 0-9/A-Z
		isNumDigit := func(b byte) bool {
			return (b >= '0' && b <= '9') || (b >= 'A' && b <= 'Z')
		}
		if isNumDigit(c) || (c == '.' && i+1 < len(src) && isNumDigit(src[i+1])) {
			start := i
			sawDot := false
			for i < len(src) {
				if isNumDigit(src[i]) {
					i++
					continue
				}
				if src[i] == '.' && !sawDot {
					sawDot = true
					i++
					continue
				}
				break
			}
			toks = append(toks, bcToken{kind: bcTokNum, val: src[start:i], line: line})
			continue
		}

		if c == '.' {
			toks = append(toks, bcToken{kind: bcTokDot, val: ".", line: line})
			i++
			continue
		}

		// Identifier / keyword: [a-z_][a-z0-9_]*
		if (c >= 'a' && c <= 'z') || c == '_' {
			start := i
			i++
			for i < len(src) && ((src[i] >= 'a' && src[i] <= 'z') || (src[i] >= '0' && src[i] <= '9') || src[i] == '_') {
				i++
			}
			toks = append(toks, bcToken{kind: bcTokIdent, val: src[start:i], line: line})
			continue
		}

		// Punctuation & operators
		switch c {
		case ';':
			toks = append(toks, bcToken{kind: bcTokSemi, val: ";", line: line})
			i++
		case ',':
			toks = append(toks, bcToken{kind: bcTokComma, val: ",", line: line})
			i++
		case '(':
			toks = append(toks, bcToken{kind: bcTokLParen, val: "(", line: line})
			i++
		case ')':
			toks = append(toks, bcToken{kind: bcTokRParen, val: ")", line: line})
			i++
		case '{':
			toks = append(toks, bcToken{kind: bcTokLBrace, val: "{", line: line})
			i++
		case '}':
			toks = append(toks, bcToken{kind: bcTokRBrace, val: "}", line: line})
			i++
		case '[':
			toks = append(toks, bcToken{kind: bcTokLBrack, val: "[", line: line})
			i++
		case ']':
			toks = append(toks, bcToken{kind: bcTokRBrack, val: "]", line: line})
			i++
		default:
			if i+1 < len(src) {
				two := src[i : i+2]
				switch two {
				case "++", "--", "+=", "-=", "*=", "/=", "%=", "^=", "==", "!=", "<=", ">=", "&&", "||":
					toks = append(toks, bcToken{kind: bcTokOp, val: two, line: line})
					i += 2
					continue
				}
			}
			switch c {
			case '+', '-', '*', '/', '%', '^', '=', '<', '>', '!':
				toks = append(toks, bcToken{kind: bcTokOp, val: string(c), line: line})
				i++
			default:
				return nil, fmt.Errorf("syntax error at line %d: unexpected character %q", line, c)
			}
		}
	}
	toks = append(toks, bcToken{kind: bcTokEOF, line: line})
	return toks, nil
}

// ---- AST & Parser ----

type bcStmt interface{ isBcStmt() }
type bcExpr interface{ isBcExpr() }

type bcExprStmt struct {
	expr       bcExpr
	isAssignOp bool
}
type bcStrStmt struct{ text string }
type bcPrintStmt struct{ items []bcExpr }
type bcBlockStmt struct{ stmts []bcStmt }
type bcIfStmt struct {
	cond     bcExpr
	thenStmt bcStmt
	elseStmt bcStmt
}
type bcWhileStmt struct {
	cond bcExpr
	body bcStmt
}
type bcForStmt struct {
	init bcExpr
	cond bcExpr
	post bcExpr
	body bcStmt
}
type bcBreakStmt struct{}
type bcContinueStmt struct{}
type bcReturnStmt struct{ expr bcExpr }
type bcQuitStmt struct{}

func (*bcExprStmt) isBcStmt()     {}
func (*bcStrStmt) isBcStmt()      {}
func (*bcPrintStmt) isBcStmt()    {}
func (*bcBlockStmt) isBcStmt()    {}
func (*bcIfStmt) isBcStmt()       {}
func (*bcWhileStmt) isBcStmt()    {}
func (*bcForStmt) isBcStmt()      {}
func (*bcBreakStmt) isBcStmt()    {}
func (*bcContinueStmt) isBcStmt() {}
func (*bcReturnStmt) isBcStmt()   {}
func (*bcQuitStmt) isBcStmt()     {}

type bcParam struct {
	name    string
	isArray bool
	byRef   bool
}

type bcFuncDef struct {
	name   string
	params []bcParam
	autos  []bcParam
	body   []bcStmt
}

type bcNumLitExpr struct{ raw string }
type bcStrLitExpr struct{ text string }
type bcVarExpr struct {
	name  string
	index bcExpr // non-nil for array element a[index]
}
type bcAssignExpr struct {
	op     string // "=", "+=", "-=", "*=", "/=", "%=", "^="
	target *bcVarExpr
	val    bcExpr
}
type bcIncDecExpr struct {
	op     string // "++", "--"
	prefix bool
	target *bcVarExpr
}
type bcUnaryExpr struct {
	op   string // "-", "+", "!"
	expr bcExpr
}
type bcBinExpr struct {
	op    string
	left  bcExpr
	right bcExpr
}
type bcCallExpr struct {
	name string
	args []bcExpr
}
type bcArrayRefArg struct{ name string } // for passing `a[]` as argument

func (*bcNumLitExpr) isBcExpr()  {}
func (*bcStrLitExpr) isBcExpr()  {}
func (*bcVarExpr) isBcExpr()     {}
func (*bcAssignExpr) isBcExpr()  {}
func (*bcIncDecExpr) isBcExpr()  {}
func (*bcUnaryExpr) isBcExpr()   {}
func (*bcBinExpr) isBcExpr()     {}
func (*bcCallExpr) isBcExpr()    {}
func (*bcArrayRefArg) isBcExpr() {}

type bcParser struct {
	toks  []bcToken
	pos   int
	funcs map[string]*bcFuncDef
}

func (p *bcParser) cur() bcToken {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return bcToken{kind: bcTokEOF}
}

func (p *bcParser) next() bcToken {
	t := p.cur()
	if p.pos < len(p.toks) {
		p.pos++
	}
	return t
}

func (p *bcParser) skipNLs() {
	for p.cur().kind == bcTokNL {
		p.pos++
	}
}

func (p *bcParser) skipSeps() {
	for p.cur().kind == bcTokNL || p.cur().kind == bcTokSemi {
		p.pos++
	}
}

func (p *bcParser) expect(k bcTokKind, desc string) (bcToken, error) {
	t := p.cur()
	if t.kind != k {
		return t, fmt.Errorf("syntax error at line %d: expected %s, got %q", t.line, desc, t.val)
	}
	p.pos++
	return t, nil
}

func (p *bcParser) parseProgram() ([]bcStmt, error) {
	var stmts []bcStmt
	for {
		p.skipSeps()
		if p.cur().kind == bcTokEOF {
			break
		}
		if p.cur().kind == bcTokIdent && p.cur().val == "define" {
			fn, err := p.parseDefine()
			if err != nil {
				return nil, err
			}
			p.funcs[fn.name] = fn
			continue
		}
		st, err := p.parseStmt()
		if err != nil {
			return nil, err
		}
		if st != nil {
			stmts = append(stmts, st)
		}
	}
	return stmts, nil
}

func (p *bcParser) parseDefine() (*bcFuncDef, error) {
	p.next() // consume "define"
	p.skipNLs()
	// Optional GNU `void` keyword
	if p.cur().kind == bcTokIdent && p.cur().val == "void" {
		p.next()
		p.skipNLs()
	}
	nameTok, err := p.expect(bcTokIdent, "function name")
	if err != nil {
		return nil, err
	}
	p.skipNLs()
	if _, err := p.expect(bcTokLParen, "("); err != nil {
		return nil, err
	}
	var params []bcParam
	p.skipNLs()
	if p.cur().kind != bcTokRParen {
		for {
			p.skipNLs()
			byRef := false
			if p.cur().kind == bcTokOp && p.cur().val == "*" {
				byRef = true
				p.next()
			}
			pt, err := p.expect(bcTokIdent, "parameter name")
			if err != nil {
				return nil, err
			}
			isArr := false
			if p.cur().kind == bcTokLBrack {
				p.next()
				if _, err := p.expect(bcTokRBrack, "]"); err != nil {
					return nil, err
				}
				isArr = true
			}
			params = append(params, bcParam{name: pt.val, isArray: isArr, byRef: byRef})
			p.skipNLs()
			if p.cur().kind == bcTokComma {
				p.next()
				continue
			}
			break
		}
	}
	if _, err := p.expect(bcTokRParen, ")"); err != nil {
		return nil, err
	}
	p.skipNLs()
	if _, err := p.expect(bcTokLBrace, "{"); err != nil {
		return nil, err
	}
	p.skipSeps()

	var autos []bcParam
	for p.cur().kind == bcTokIdent && p.cur().val == "auto" {
		p.next()
		for {
			at, err := p.expect(bcTokIdent, "auto variable name")
			if err != nil {
				return nil, err
			}
			isArr := false
			if p.cur().kind == bcTokLBrack {
				p.next()
				if _, err := p.expect(bcTokRBrack, "]"); err != nil {
					return nil, err
				}
				isArr = true
			}
			autos = append(autos, bcParam{name: at.val, isArray: isArr})
			if p.cur().kind == bcTokComma {
				p.next()
				continue
			}
			break
		}
		if p.cur().kind != bcTokSemi && p.cur().kind != bcTokNL && p.cur().kind != bcTokRBrace {
			return nil, fmt.Errorf("syntax error at line %d: expected ';' or newline after auto", p.cur().line)
		}
		p.skipSeps()
	}

	var body []bcStmt
	for {
		p.skipSeps()
		if p.cur().kind == bcTokRBrace {
			p.next()
			break
		}
		if p.cur().kind == bcTokEOF {
			return nil, fmt.Errorf("syntax error: unexpected EOF inside define %s", nameTok.val)
		}
		st, err := p.parseStmt()
		if err != nil {
			return nil, err
		}
		if st != nil {
			body = append(body, st)
		}
	}
	return &bcFuncDef{name: nameTok.val, params: params, autos: autos, body: body}, nil
}

func (p *bcParser) parseStmt() (bcStmt, error) {
	p.skipNLs()
	t := p.cur()
	switch t.kind {
	case bcTokSemi:
		p.next()
		return nil, nil
	case bcTokLBrace:
		p.next()
		var list []bcStmt
		for {
			p.skipSeps()
			if p.cur().kind == bcTokRBrace {
				p.next()
				break
			}
			if p.cur().kind == bcTokEOF {
				return nil, fmt.Errorf("syntax error at line %d: unclosed '{'", t.line)
			}
			st, err := p.parseStmt()
			if err != nil {
				return nil, err
			}
			if st != nil {
				list = append(list, st)
			}
		}
		return &bcBlockStmt{stmts: list}, nil

	case bcTokStr:
		p.next()
		return &bcStrStmt{text: t.val}, nil

	case bcTokIdent:
		switch t.val {
		case "if":
			p.next()
			p.skipNLs()
			if _, err := p.expect(bcTokLParen, "("); err != nil {
				return nil, err
			}
			p.skipNLs()
			cond, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			p.skipNLs()
			if _, err := p.expect(bcTokRParen, ")"); err != nil {
				return nil, err
			}
			p.skipNLs()
			thenSt, err := p.parseStmt()
			if err != nil {
				return nil, err
			}
			// Check optional `else` (possibly after `;` or `\n`)
			savePos := p.pos
			p.skipSeps()
			var elseSt bcStmt
			if p.cur().kind == bcTokIdent && p.cur().val == "else" {
				p.next()
				p.skipNLs()
				elseSt, err = p.parseStmt()
				if err != nil {
					return nil, err
				}
			} else {
				p.pos = savePos
			}
			return &bcIfStmt{cond: cond, thenStmt: thenSt, elseStmt: elseSt}, nil

		case "while":
			p.next()
			p.skipNLs()
			if _, err := p.expect(bcTokLParen, "("); err != nil {
				return nil, err
			}
			p.skipNLs()
			cond, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			p.skipNLs()
			if _, err := p.expect(bcTokRParen, ")"); err != nil {
				return nil, err
			}
			p.skipNLs()
			body, err := p.parseStmt()
			if err != nil {
				return nil, err
			}
			return &bcWhileStmt{cond: cond, body: body}, nil

		case "for":
			p.next()
			p.skipNLs()
			if _, err := p.expect(bcTokLParen, "("); err != nil {
				return nil, err
			}
			p.skipNLs()
			var initExpr, condExpr, postExpr bcExpr
			var err error
			if p.cur().kind != bcTokSemi {
				initExpr, err = p.parseExpr()
				if err != nil {
					return nil, err
				}
			}
			if _, err := p.expect(bcTokSemi, ";"); err != nil {
				return nil, err
			}
			p.skipNLs()
			if p.cur().kind != bcTokSemi {
				condExpr, err = p.parseExpr()
				if err != nil {
					return nil, err
				}
			}
			if _, err := p.expect(bcTokSemi, ";"); err != nil {
				return nil, err
			}
			p.skipNLs()
			if p.cur().kind != bcTokRParen {
				postExpr, err = p.parseExpr()
				if err != nil {
					return nil, err
				}
			}
			p.skipNLs()
			if _, err := p.expect(bcTokRParen, ")"); err != nil {
				return nil, err
			}
			p.skipNLs()
			body, err := p.parseStmt()
			if err != nil {
				return nil, err
			}
			return &bcForStmt{init: initExpr, cond: condExpr, post: postExpr, body: body}, nil

		case "break":
			p.next()
			return &bcBreakStmt{}, nil
		case "continue":
			p.next()
			return &bcContinueStmt{}, nil
		case "quit", "halt":
			p.next()
			return &bcQuitStmt{}, nil
		case "return":
			p.next()
			if p.cur().kind == bcTokSemi || p.cur().kind == bcTokNL || p.cur().kind == bcTokRBrace || p.cur().kind == bcTokEOF {
				return &bcReturnStmt{}, nil
			}
			ex, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			return &bcReturnStmt{expr: ex}, nil
		case "print":
			p.next()
			var items []bcExpr
			for {
				if p.cur().kind == bcTokStr {
					st := p.next()
					items = append(items, &bcStrLitExpr{text: st.val})
				} else {
					ex, err := p.parseExpr()
					if err != nil {
						return nil, err
					}
					items = append(items, ex)
				}
				if p.cur().kind == bcTokComma {
					p.next()
					p.skipNLs()
					continue
				}
				break
			}
			return &bcPrintStmt{items: items}, nil
		}
	}

	ex, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	_, isAssign := ex.(*bcAssignExpr)
	if p.cur().kind != bcTokSemi && p.cur().kind != bcTokNL && p.cur().kind != bcTokRBrace && p.cur().kind != bcTokEOF {
		// Check for `else` immediately following a single-statement `if` branch
		if !(p.cur().kind == bcTokIdent && p.cur().val == "else") {
			return nil, fmt.Errorf("syntax error at line %d: unexpected token %q", p.cur().line, p.cur().val)
		}
	}
	if p.cur().kind == bcTokSemi || p.cur().kind == bcTokNL {
		p.next()
	}
	return &bcExprStmt{expr: ex, isAssignOp: isAssign}, nil
}

func (p *bcParser) parseExpr() (bcExpr, error) {
	return p.parseAssign()
}

func isBcAssignOp(op string) bool {
	switch op {
	case "=", "+=", "-=", "*=", "/=", "%=", "^=":
		return true
	}
	return false
}

func (p *bcParser) parseAssign() (bcExpr, error) {
	left, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.cur().kind == bcTokOp && isBcAssignOp(p.cur().val) {
		op := p.next().val
		v, ok := left.(*bcVarExpr)
		if !ok {
			return nil, fmt.Errorf("syntax error at line %d: invalid assignment target", p.cur().line)
		}
		p.skipNLs()
		right, err := p.parseAssign()
		if err != nil {
			return nil, err
		}
		return &bcAssignExpr{op: op, target: v, val: right}, nil
	}
	return left, nil
}

func (p *bcParser) parseOr() (bcExpr, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.cur().kind == bcTokOp && p.cur().val == "||" {
		p.next()
		p.skipNLs()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &bcBinExpr{op: "||", left: left, right: right}
	}
	return left, nil
}

func (p *bcParser) parseAnd() (bcExpr, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.cur().kind == bcTokOp && p.cur().val == "&&" {
		p.next()
		p.skipNLs()
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		left = &bcBinExpr{op: "&&", left: left, right: right}
	}
	return left, nil
}

func (p *bcParser) parseNot() (bcExpr, error) {
	if p.cur().kind == bcTokOp && p.cur().val == "!" {
		p.next()
		sub, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return &bcUnaryExpr{op: "!", expr: sub}, nil
	}
	return p.parseRel()
}

func (p *bcParser) parseRel() (bcExpr, error) {
	left, err := p.parseAdd()
	if err != nil {
		return nil, err
	}
	if p.cur().kind == bcTokOp {
		switch p.cur().val {
		case "==", "!=", "<=", ">=", "<", ">":
			op := p.next().val
			p.skipNLs()
			right, err := p.parseAdd()
			if err != nil {
				return nil, err
			}
			return &bcBinExpr{op: op, left: left, right: right}, nil
		}
	}
	return left, nil
}

func (p *bcParser) parseAdd() (bcExpr, error) {
	left, err := p.parseMul()
	if err != nil {
		return nil, err
	}
	for p.cur().kind == bcTokOp && (p.cur().val == "+" || p.cur().val == "-") {
		op := p.next().val
		p.skipNLs()
		right, err := p.parseMul()
		if err != nil {
			return nil, err
		}
		left = &bcBinExpr{op: op, left: left, right: right}
	}
	return left, nil
}

func (p *bcParser) parseMul() (bcExpr, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for p.cur().kind == bcTokOp && (p.cur().val == "*" || p.cur().val == "/" || p.cur().val == "%") {
		op := p.next().val
		p.skipNLs()
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = &bcBinExpr{op: op, left: left, right: right}
	}
	return left, nil
}

func (p *bcParser) parseUnary() (bcExpr, error) {
	if p.cur().kind == bcTokOp && (p.cur().val == "-" || p.cur().val == "+" || p.cur().val == "!") {
		op := p.next().val
		sub, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &bcUnaryExpr{op: op, expr: sub}, nil
	}
	return p.parsePower()
}

func (p *bcParser) parsePower() (bcExpr, error) {
	base, err := p.parseIncDec()
	if err != nil {
		return nil, err
	}
	if p.cur().kind == bcTokOp && p.cur().val == "^" {
		p.next()
		p.skipNLs()
		exp, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &bcBinExpr{op: "^", left: base, right: exp}, nil
	}
	return base, nil
}

func (p *bcParser) parseIncDec() (bcExpr, error) {
	if p.cur().kind == bcTokOp && (p.cur().val == "++" || p.cur().val == "--") {
		op := p.next().val
		prim, err := p.parsePrimary()
		if err != nil {
			return nil, err
		}
		v, ok := prim.(*bcVarExpr)
		if !ok {
			return nil, fmt.Errorf("syntax error at line %d: invalid %s operand", p.cur().line, op)
		}
		return &bcIncDecExpr{op: op, prefix: true, target: v}, nil
	}
	prim, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	if p.cur().kind == bcTokOp && (p.cur().val == "++" || p.cur().val == "--") {
		op := p.next().val
		v, ok := prim.(*bcVarExpr)
		if !ok {
			return nil, fmt.Errorf("syntax error at line %d: invalid %s operand", p.cur().line, op)
		}
		return &bcIncDecExpr{op: op, prefix: false, target: v}, nil
	}
	return prim, nil
}

func (p *bcParser) parsePrimary() (bcExpr, error) {
	t := p.cur()
	switch t.kind {
	case bcTokNum:
		p.next()
		return &bcNumLitExpr{raw: t.val}, nil
	case bcTokDot:
		p.next()
		return &bcVarExpr{name: "last"}, nil
	case bcTokLParen:
		p.next()
		p.skipNLs()
		ex, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		p.skipNLs()
		if _, err := p.expect(bcTokRParen, ")"); err != nil {
			return nil, err
		}
		return ex, nil
	case bcTokIdent:
		p.next()
		// Function call: fname(...) — note `scale(x)` is function call whereas `scale` or `scale = 10` is variable!
		if p.cur().kind == bcTokLParen {
			p.next()
			var args []bcExpr
			p.skipNLs()
			if p.cur().kind != bcTokRParen {
				for {
					p.skipNLs()
					// Check array argument `arr[]`
					if p.cur().kind == bcTokIdent && p.pos+2 < len(p.toks) &&
						p.toks[p.pos+1].kind == bcTokLBrack && p.toks[p.pos+2].kind == bcTokRBrack {
						arrName := p.next().val
						p.next() // [
						p.next() // ]
						args = append(args, &bcArrayRefArg{name: arrName})
					} else {
						argEx, err := p.parseExpr()
						if err != nil {
							return nil, err
						}
						args = append(args, argEx)
					}
					p.skipNLs()
					if p.cur().kind == bcTokComma {
						p.next()
						continue
					}
					break
				}
			}
			if _, err := p.expect(bcTokRParen, ")"); err != nil {
				return nil, err
			}
			return &bcCallExpr{name: t.val, args: args}, nil
		}
		// Array subscript: arr[expr]
		if p.cur().kind == bcTokLBrack {
			p.next()
			p.skipNLs()
			idxEx, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			p.skipNLs()
			if _, err := p.expect(bcTokRBrack, "]"); err != nil {
				return nil, err
			}
			return &bcVarExpr{name: t.val, index: idxEx}, nil
		}
		return &bcVarExpr{name: t.val}, nil
	default:
		return nil, fmt.Errorf("syntax error at line %d: unexpected token %q", t.line, t.val)
	}
}

// ---- Runtime Evaluator ----

type bcSignal int

const (
	bcSigNone bcSignal = iota
	bcSigBreak
	bcSigContinue
	bcSigReturn
	bcSigQuit
)

type bcFrame struct {
	vars   map[string]bcNum
	arrays map[string]map[string]bcNum
}

type bcVM struct {
	ctx     context.Context
	stdout  io.Writer
	stderr  io.Writer
	mathlib bool

	scale int
	ibase int
	obase int
	last  bcNum

	globals map[string]bcNum
	gArrays map[string]map[string]bcNum
	frames  []bcFrame
	funcs   map[string]*bcFuncDef

	hadRuntimeErr bool
}

func newBcVM(ctx context.Context, stdout, stderr io.Writer, mathlib bool) *bcVM {
	sc := 0
	if mathlib {
		sc = 20
	}
	return &bcVM{
		ctx:     ctx,
		stdout:  stdout,
		stderr:  stderr,
		mathlib: mathlib,
		scale:   sc,
		ibase:   10,
		obase:   10,
		last:    bcZero(),
		globals: make(map[string]bcNum),
		gArrays: make(map[string]map[string]bcNum),
		funcs:   make(map[string]*bcFuncDef),
	}
}

func (vm *bcVM) getScalar(name string) bcNum {
	switch name {
	case "scale":
		return bcFromInt64(int64(vm.scale))
	case "ibase":
		return bcFromInt64(int64(vm.ibase))
	case "obase":
		return bcFromInt64(int64(vm.obase))
	case "last":
		return vm.last.clone()
	}
	for i := len(vm.frames) - 1; i >= 0; i-- {
		if v, ok := vm.frames[i].vars[name]; ok {
			return v.clone()
		}
	}
	if v, ok := vm.globals[name]; ok {
		return v.clone()
	}
	return bcZero()
}

func (vm *bcVM) setScalar(name string, val bcNum) error {
	switch name {
	case "scale":
		iv, ok := val.truncInt64()
		if !ok || iv < 0 || iv > 100000 {
			return errors.New("Runtime error: bad scale value")
		}
		vm.scale = int(iv)
		return nil
	case "ibase":
		iv, ok := val.truncInt64()
		if !ok || iv < 2 || iv > 36 {
			return errors.New("Runtime error: bad ibase value (must be 2..36)")
		}
		vm.ibase = int(iv)
		return nil
	case "obase":
		iv, ok := val.truncInt64()
		if !ok || iv < 2 || iv > 1_000_000 {
			return errors.New("Runtime error: bad obase value (must be >= 2)")
		}
		vm.obase = int(iv)
		return nil
	case "last":
		vm.last = val.clone()
		return nil
	}
	for i := len(vm.frames) - 1; i >= 0; i-- {
		if _, ok := vm.frames[i].vars[name]; ok {
			vm.frames[i].vars[name] = val.clone()
			return nil
		}
	}
	vm.globals[name] = val.clone()
	return nil
}

func (vm *bcVM) resolveArrayMap(name string, create bool) map[string]bcNum {
	for i := len(vm.frames) - 1; i >= 0; i-- {
		if m, ok := vm.frames[i].arrays[name]; ok {
			return m
		}
	}
	m, ok := vm.gArrays[name]
	if !ok && create {
		m = make(map[string]bcNum)
		vm.gArrays[name] = m
	}
	return m
}

func (vm *bcVM) evalArrayKey(expr bcExpr) (string, error) {
	idxVal, err := vm.evalExpr(expr)
	if err != nil {
		return "", err
	}
	iv, ok := idxVal.truncInt64()
	if !ok || iv < 0 {
		return "", errors.New("Runtime error: negative or invalid array index")
	}
	return strconv.FormatInt(iv, 10), nil
}

func (vm *bcVM) getVar(v *bcVarExpr) (bcNum, error) {
	if v.index == nil {
		return vm.getScalar(v.name), nil
	}
	key, err := vm.evalArrayKey(v.index)
	if err != nil {
		return bcZero(), err
	}
	m := vm.resolveArrayMap(v.name, false)
	if m == nil {
		return bcZero(), nil
	}
	if val, ok := m[key]; ok {
		return val.clone(), nil
	}
	return bcZero(), nil
}

func (vm *bcVM) setVarKey(name, key string, isArr bool, val bcNum) error {
	if !isArr {
		return vm.setScalar(name, val)
	}
	m := vm.resolveArrayMap(name, true)
	m[key] = val.clone()
	return nil
}

func (vm *bcVM) writeOut(s string) error {
	_, err := io.WriteString(vm.stdout, s)
	return err
}

func unescapeBcPrintStr(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'b':
				b.WriteByte('\b')
			case 'a':
				b.WriteByte('\a')
			case 'f':
				b.WriteByte('\f')
			case 'q', '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			default:
				b.WriteByte('\\')
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func (vm *bcVM) execStmt(st bcStmt) (bcSignal, bcNum, error) {
	if vm.ctx.Err() != nil {
		return bcSigQuit, bcZero(), vm.ctx.Err()
	}
	if st == nil {
		return bcSigNone, bcZero(), nil
	}
	switch s := st.(type) {
	case *bcExprStmt:
		val, err := vm.evalExpr(s.expr)
		if err != nil {
			return bcSigNone, bcZero(), err
		}
		if !s.isAssignOp {
			vm.last = val.clone()
			if werr := vm.writeOut(formatBcNum(val, vm.obase) + "\n"); werr != nil {
				return bcSigQuit, bcZero(), werr
			}
		}
		return bcSigNone, bcZero(), nil

	case *bcStrStmt:
		if werr := vm.writeOut(s.text); werr != nil {
			return bcSigQuit, bcZero(), werr
		}
		return bcSigNone, bcZero(), nil

	case *bcPrintStmt:
		for _, item := range s.items {
			if sl, ok := item.(*bcStrLitExpr); ok {
				if werr := vm.writeOut(unescapeBcPrintStr(sl.text)); werr != nil {
					return bcSigQuit, bcZero(), werr
				}
			} else {
				v, err := vm.evalExpr(item)
				if err != nil {
					return bcSigNone, bcZero(), err
				}
				vm.last = v.clone()
				if werr := vm.writeOut(formatBcNum(v, vm.obase)); werr != nil {
					return bcSigQuit, bcZero(), werr
				}
			}
		}
		return bcSigNone, bcZero(), nil

	case *bcBlockStmt:
		for _, sub := range s.stmts {
			sig, rv, err := vm.execStmt(sub)
			if err != nil || sig != bcSigNone {
				return sig, rv, err
			}
		}
		return bcSigNone, bcZero(), nil

	case *bcIfStmt:
		cv, err := vm.evalExpr(s.cond)
		if err != nil {
			return bcSigNone, bcZero(), err
		}
		if !cv.isZero() {
			return vm.execStmt(s.thenStmt)
		} else if s.elseStmt != nil {
			return vm.execStmt(s.elseStmt)
		}
		return bcSigNone, bcZero(), nil

	case *bcWhileStmt:
		for {
			if vm.ctx.Err() != nil {
				return bcSigQuit, bcZero(), vm.ctx.Err()
			}
			cv, err := vm.evalExpr(s.cond)
			if err != nil {
				return bcSigNone, bcZero(), err
			}
			if cv.isZero() {
				break
			}
			sig, rv, err := vm.execStmt(s.body)
			if err != nil {
				return bcSigNone, bcZero(), err
			}
			switch sig {
			case bcSigBreak:
				return bcSigNone, bcZero(), nil
			case bcSigContinue:
				continue
			case bcSigReturn, bcSigQuit:
				return sig, rv, nil
			}
		}
		return bcSigNone, bcZero(), nil

	case *bcForStmt:
		if s.init != nil {
			if _, err := vm.evalExpr(s.init); err != nil {
				return bcSigNone, bcZero(), err
			}
		}
		for {
			if vm.ctx.Err() != nil {
				return bcSigQuit, bcZero(), vm.ctx.Err()
			}
			if s.cond != nil {
				cv, err := vm.evalExpr(s.cond)
				if err != nil {
					return bcSigNone, bcZero(), err
				}
				if cv.isZero() {
					break
				}
			}
			sig, rv, err := vm.execStmt(s.body)
			if err != nil {
				return bcSigNone, bcZero(), err
			}
			if sig == bcSigBreak {
				return bcSigNone, bcZero(), nil
			}
			if sig == bcSigReturn || sig == bcSigQuit {
				return sig, rv, nil
			}
			if s.post != nil {
				if _, err := vm.evalExpr(s.post); err != nil {
					return bcSigNone, bcZero(), err
				}
			}
		}
		return bcSigNone, bcZero(), nil

	case *bcBreakStmt:
		return bcSigBreak, bcZero(), nil
	case *bcContinueStmt:
		return bcSigContinue, bcZero(), nil
	case *bcQuitStmt:
		return bcSigQuit, bcZero(), nil
	case *bcReturnStmt:
		if s.expr == nil {
			return bcSigReturn, bcZero(), nil
		}
		rv, err := vm.evalExpr(s.expr)
		if err != nil {
			return bcSigNone, bcZero(), err
		}
		return bcSigReturn, rv, nil
	}
	return bcSigNone, bcZero(), nil
}

func (vm *bcVM) evalExpr(expr bcExpr) (bcNum, error) {
	if vm.ctx.Err() != nil {
		return bcZero(), vm.ctx.Err()
	}
	switch e := expr.(type) {
	case *bcNumLitExpr:
		return parseBcLiteral(e.raw, vm.ibase), nil

	case *bcVarExpr:
		return vm.getVar(e)

	case *bcAssignExpr:
		isArr := e.target.index != nil
		var key string
		if isArr {
			var err error
			key, err = vm.evalArrayKey(e.target.index)
			if err != nil {
				return bcZero(), err
			}
		}
		rhs, err := vm.evalExpr(e.val)
		if err != nil {
			return bcZero(), err
		}
		var newVal bcNum
		if e.op == "=" {
			newVal = rhs
		} else {
			var cur bcNum
			if isArr {
				if m := vm.resolveArrayMap(e.target.name, false); m != nil {
					if v, ok := m[key]; ok {
						cur = v.clone()
					} else {
						cur = bcZero()
					}
				} else {
					cur = bcZero()
				}
			} else {
				cur = vm.getScalar(e.target.name)
			}
			newVal, err = vm.applyBinArith(e.op[:1], cur, rhs)
			if err != nil {
				return bcZero(), err
			}
		}
		if err := vm.setVarKey(e.target.name, key, isArr, newVal); err != nil {
			return bcZero(), err
		}
		return newVal, nil

	case *bcIncDecExpr:
		isArr := e.target.index != nil
		var key string
		var cur bcNum
		if isArr {
			var err error
			key, err = vm.evalArrayKey(e.target.index)
			if err != nil {
				return bcZero(), err
			}
			if m := vm.resolveArrayMap(e.target.name, false); m != nil {
				if v, ok := m[key]; ok {
					cur = v.clone()
				} else {
					cur = bcZero()
				}
			} else {
				cur = bcZero()
			}
		} else {
			cur = vm.getScalar(e.target.name)
		}
		var updated bcNum
		if e.op == "++" {
			updated = bcAdd(cur, bcFromInt64(1))
		} else {
			updated = bcSub(cur, bcFromInt64(1))
		}
		if err := vm.setVarKey(e.target.name, key, isArr, updated); err != nil {
			return bcZero(), err
		}
		if e.prefix {
			return updated, nil
		}
		return cur, nil

	case *bcUnaryExpr:
		sub, err := vm.evalExpr(e.expr)
		if err != nil {
			return bcZero(), err
		}
		switch e.op {
		case "-":
			return bcNeg(sub), nil
		case "+":
			return sub, nil
		case "!":
			if sub.isZero() {
				return bcFromInt64(1), nil
			}
			return bcFromInt64(0), nil
		}

	case *bcBinExpr:
		// Short-circuit boolean operators
		if e.op == "&&" {
			lv, err := vm.evalExpr(e.left)
			if err != nil {
				return bcZero(), err
			}
			if lv.isZero() {
				return bcFromInt64(0), nil
			}
			rv, err := vm.evalExpr(e.right)
			if err != nil {
				return bcZero(), err
			}
			if rv.isZero() {
				return bcFromInt64(0), nil
			}
			return bcFromInt64(1), nil
		}
		if e.op == "||" {
			lv, err := vm.evalExpr(e.left)
			if err != nil {
				return bcZero(), err
			}
			if !lv.isZero() {
				return bcFromInt64(1), nil
			}
			rv, err := vm.evalExpr(e.right)
			if err != nil {
				return bcZero(), err
			}
			if !rv.isZero() {
				return bcFromInt64(1), nil
			}
			return bcFromInt64(0), nil
		}

		lv, err := vm.evalExpr(e.left)
		if err != nil {
			return bcZero(), err
		}
		rv, err := vm.evalExpr(e.right)
		if err != nil {
			return bcZero(), err
		}
		switch e.op {
		case "==":
			return boolToBc(bcCmp(lv, rv) == 0), nil
		case "!=":
			return boolToBc(bcCmp(lv, rv) != 0), nil
		case "<":
			return boolToBc(bcCmp(lv, rv) < 0), nil
		case "<=":
			return boolToBc(bcCmp(lv, rv) <= 0), nil
		case ">":
			return boolToBc(bcCmp(lv, rv) > 0), nil
		case ">=":
			return boolToBc(bcCmp(lv, rv) >= 0), nil
		default:
			return vm.applyBinArith(e.op, lv, rv)
		}

	case *bcCallExpr:
		return vm.evalCall(e)
	}
	return bcZero(), nil
}

func boolToBc(b bool) bcNum {
	if b {
		return bcFromInt64(1)
	}
	return bcFromInt64(0)
}

func (vm *bcVM) applyBinArith(op string, lv, rv bcNum) (bcNum, error) {
	switch op {
	case "+":
		return bcAdd(lv, rv), nil
	case "-":
		return bcSub(lv, rv), nil
	case "*":
		return bcMul(lv, rv, vm.scale), nil
	case "/":
		return bcDiv(lv, rv, vm.scale)
	case "%":
		return bcMod(lv, rv, vm.scale)
	case "^":
		return bcPow(vm.ctx, lv, rv, vm.scale)
	}
	return bcZero(), fmt.Errorf("Runtime error: unknown operator %s", op)
}

func (vm *bcVM) evalCall(c *bcCallExpr) (bcNum, error) {
	// Built-in functions: sqrt, length, scale
	switch c.name {
	case "sqrt":
		if len(c.args) != 1 {
			return bcZero(), errors.New("Runtime error: sqrt() requires 1 argument")
		}
		v, err := vm.evalExpr(c.args[0])
		if err != nil {
			return bcZero(), err
		}
		return bcSqrt(v, vm.scale)
	case "length":
		if len(c.args) != 1 {
			return bcZero(), errors.New("Runtime error: length() requires 1 argument")
		}
		if ar, ok := c.args[0].(*bcArrayRefArg); ok {
			m := vm.resolveArrayMap(ar.name, false)
			return bcFromInt64(int64(len(m))), nil
		}
		v, err := vm.evalExpr(c.args[0])
		if err != nil {
			return bcZero(), err
		}
		return bcLength(v), nil
	case "scale":
		if len(c.args) != 1 {
			return bcZero(), errors.New("Runtime error: scale() requires 1 argument")
		}
		v, err := vm.evalExpr(c.args[0])
		if err != nil {
			return bcZero(), err
		}
		return bcFromInt64(int64(v.sc)), nil
	}

	// User-defined functions take priority over -l mathlib if user redefined s/c/a/l/e/j!
	if fn, ok := vm.funcs[c.name]; ok {
		return vm.callUserFunc(fn, c.args)
	}

	if vm.mathlib {
		switch c.name {
		case "s":
			if len(c.args) != 1 {
				return bcZero(), errors.New("Runtime error: s(x) requires 1 argument")
			}
			v, err := vm.evalExpr(c.args[0])
			if err != nil {
				return bcZero(), err
			}
			return bcMathSinCos(vm.ctx, v, false, vm.scale)
		case "c":
			if len(c.args) != 1 {
				return bcZero(), errors.New("Runtime error: c(x) requires 1 argument")
			}
			v, err := vm.evalExpr(c.args[0])
			if err != nil {
				return bcZero(), err
			}
			return bcMathSinCos(vm.ctx, v, true, vm.scale)
		case "a":
			if len(c.args) != 1 {
				return bcZero(), errors.New("Runtime error: a(x) requires 1 argument")
			}
			v, err := vm.evalExpr(c.args[0])
			if err != nil {
				return bcZero(), err
			}
			return bcMathAtan(vm.ctx, v, vm.scale)
		case "l":
			if len(c.args) != 1 {
				return bcZero(), errors.New("Runtime error: l(x) requires 1 argument")
			}
			v, err := vm.evalExpr(c.args[0])
			if err != nil {
				return bcZero(), err
			}
			return bcMathLn(vm.ctx, v, vm.scale)
		case "e":
			if len(c.args) != 1 {
				return bcZero(), errors.New("Runtime error: e(x) requires 1 argument")
			}
			v, err := vm.evalExpr(c.args[0])
			if err != nil {
				return bcZero(), err
			}
			return bcMathExp(vm.ctx, v, vm.scale)
		case "j":
			if len(c.args) != 2 {
				return bcZero(), errors.New("Runtime error: j(n, x) requires 2 arguments")
			}
			nv, err := vm.evalExpr(c.args[0])
			if err != nil {
				return bcZero(), err
			}
			xv, err := vm.evalExpr(c.args[1])
			if err != nil {
				return bcZero(), err
			}
			return bcMathBesselJ(vm.ctx, nv, xv, vm.scale)
		}
	}

	return bcZero(), fmt.Errorf("Runtime error: function %s() is not defined", c.name)
}

func (vm *bcVM) callUserFunc(fn *bcFuncDef, rawArgs []bcExpr) (bcNum, error) {
	if len(vm.frames) >= 1500 {
		return bcZero(), errors.New("Runtime error: maximum recursion depth exceeded")
	}
	if len(rawArgs) != len(fn.params) {
		return bcZero(), fmt.Errorf("Runtime error: function %s() expects %d arguments, got %d", fn.name, len(fn.params), len(rawArgs))
	}

	frame := bcFrame{
		vars:   make(map[string]bcNum, len(fn.params)+len(fn.autos)),
		arrays: make(map[string]map[string]bcNum),
	}

	// Evaluate all arguments in caller's frame BEFORE pushing new frame!
	for i, param := range fn.params {
		if param.isArray {
			ar, ok := rawArgs[i].(*bcArrayRefArg)
			if !ok {
				if ve, vok := rawArgs[i].(*bcVarExpr); vok && ve.index == nil {
					ar = &bcArrayRefArg{name: ve.name}
					ok = true
				}
			}
			if !ok {
				return bcZero(), fmt.Errorf("Runtime error: argument %d of %s() must be an array", i+1, fn.name)
			}
			srcMap := vm.resolveArrayMap(ar.name, param.byRef)
			if param.byRef {
				frame.arrays[param.name] = srcMap
			} else {
				cp := make(map[string]bcNum, len(srcMap))
				for k, v := range srcMap {
					cp[k] = v.clone()
				}
				frame.arrays[param.name] = cp
			}
		} else {
			v, err := vm.evalExpr(rawArgs[i])
			if err != nil {
				return bcZero(), err
			}
			frame.vars[param.name] = v.clone()
		}
	}

	for _, au := range fn.autos {
		if au.isArray {
			frame.arrays[au.name] = make(map[string]bcNum)
		} else {
			frame.vars[au.name] = bcZero()
		}
	}

	vm.frames = append(vm.frames, frame)
	defer func() {
		vm.frames = vm.frames[:len(vm.frames)-1]
	}()

	for _, st := range fn.body {
		sig, rv, err := vm.execStmt(st)
		if err != nil {
			return bcZero(), err
		}
		if sig == bcSigReturn {
			return rv, nil
		}
		if sig == bcSigQuit {
			return bcZero(), errBcQuit
		}
	}
	return bcZero(), nil
}

var errBcQuit = errors.New("bc quit")

func (vm *bcVM) runSource(src string) (quit bool, err error) {
	toks, err := lexBc(src)
	if err != nil {
		return false, err
	}
	p := &bcParser{toks: toks, funcs: vm.funcs}
	stmts, err := p.parseProgram()
	if err != nil {
		return false, err
	}
	for _, st := range stmts {
		sig, _, rerr := vm.execStmt(st)
		if rerr != nil {
			if errors.Is(rerr, errBcQuit) {
				return true, nil
			}
			if isPipeClosed(rerr) || vm.ctx.Err() != nil {
				return true, rerr
			}
			fmt.Fprintf(vm.stderr, "bc: %v\n", rerr)
			vm.hadRuntimeErr = true
			continue
		}
		if sig == bcSigQuit {
			return true, nil
		}
	}
	return false, nil
}

// ---- CLI Entry Point: cmdBc ----

func cmdBc(ctx context.Context, hc interp.HandlerContext, args []string) error {
	mathlib := false
	var files []string
	endFlags := false

	for i := 1; i < len(args); i++ {
		a := args[i]
		if endFlags {
			files = append(files, a)
			continue
		}
		if a == "--" {
			endFlags = true
			continue
		}
		if a == "-" || !strings.HasPrefix(a, "-") {
			files = append(files, a)
			continue
		}
		if strings.HasPrefix(a, "--") {
			switch a[2:] {
			case "help":
				fmt.Fprintln(hc.Stdout, "Usage: bc [-lqws] [file ...]")
				return nil
			case "mathlib":
				mathlib = true
			case "quiet", "warn", "standard", "interactive":
			default:
				fmt.Fprintf(hc.Stderr, "bc: unrecognized option '%s'\n", a)
				return exitError{1}
			}
			continue
		}
		for k := 1; k < len(a); k++ {
			switch a[k] {
			case 'h':
				fmt.Fprintln(hc.Stdout, "Usage: bc [-lqws] [file ...]")
				return nil
			case 'l':
				mathlib = true
			case 'q', 'w', 's', 'i':
			default:
				fmt.Fprintf(hc.Stderr, "bc: invalid option -- '%c'\n", a[k])
				return exitError{1}
			}
		}
	}

	var outBuf bytes.Buffer
	vm := newBcVM(ctx, hc.Stdout, hc.Stderr, mathlib)
	_ = outBuf

	for _, f := range files {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var data []byte
		var err error
		if f == "-" || f == "/dev/stdin" {
			data, err = io.ReadAll(hc.Stdin)
		} else if isToolDevNull(f) {
			continue
		} else {
			data, err = readShellFile(resolve(hc.Dir, f))
		}
		if err != nil {
			fmt.Fprintf(hc.Stderr, "bc: %s: %v\n", f, err)
			return exitError{1}
		}
		quit, rerr := vm.runSource(string(data))
		if rerr != nil {
			if isPipeClosed(rerr) || ctx.Err() != nil {
				return rerr
			}
			fmt.Fprintf(hc.Stderr, "bc: %v\n", rerr)
			return exitError{1}
		}
		if quit {
			return nil
		}
	}

	// Stream stdin line-by-line for simple statements so `yes '2+2' | bc | head -n 1` exits immediately on EPIPE,
	// while buffering multi-line control blocks (`if`/`else`, `while`, `for`, `define`, comments, strings) safely.
	if len(files) == 0 || rgIsPipedStdin(hc.Stdin) {
		lr := newLineReader(hc.Stdin)
		var chunk strings.Builder
		var braceDepth, parenDepth int
		var inComment, inStr, hasCtrl bool

		runChunk := func() (bool, error) {
			if chunk.Len() == 0 {
				return false, nil
			}
			src := chunk.String()
			chunk.Reset()
			hasCtrl = false
			return vm.runSource(src)
		}

		for lr.Scan() {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			line := lr.Text()
			chunk.WriteString(line)
			chunk.WriteByte('\n')

			// Update lexical nesting state for this line.
			for i := 0; i < len(line); i++ {
				c := line[i]
				if inComment {
					if c == '*' && i+1 < len(line) && line[i+1] == '/' {
						inComment = false
						i++
					}
					continue
				}
				if inStr {
					if c == '"' {
						inStr = false
					}
					continue
				}
				if c == '#' {
					break
				}
				if c == '/' && i+1 < len(line) && line[i+1] == '*' {
					inComment = true
					i++
					continue
				}
				switch c {
				case '"':
					inStr = true
				case '{':
					braceDepth++
				case '}':
					if braceDepth > 0 {
						braceDepth--
					}
				case '(':
					parenDepth++
				case ')':
					if parenDepth > 0 {
						parenDepth--
					}
				default:
					if c >= 'a' && c <= 'z' {
						s := i
						for i+1 < len(line) && ((line[i+1] >= 'a' && line[i+1] <= 'z') || (line[i+1] >= '0' && line[i+1] <= '9') || line[i+1] == '_') {
							i++
						}
						switch line[s : i+1] {
						case "if", "else", "while", "for", "define":
							hasCtrl = true
						}
					}
				}
			}

			trimmed := strings.TrimRight(line, "\r")
			if !hasCtrl && braceDepth == 0 && parenDepth == 0 && !inComment && !inStr && !strings.HasSuffix(trimmed, `\`) {
				quit, rerr := runChunk()
				if rerr != nil {
					if isPipeClosed(rerr) || ctx.Err() != nil {
						return rerr
					}
					fmt.Fprintf(hc.Stderr, "bc: %v\n", rerr)
					return exitError{1}
				}
				if quit {
					return nil
				}
			}
		}
		if err := lr.Err(); err != nil {
			if isPipeClosed(err) || ctx.Err() != nil {
				return err
			}
			fmt.Fprintf(hc.Stderr, "bc: %v\n", err)
			return exitError{1}
		}
		if _, rerr := runChunk(); rerr != nil {
			if isPipeClosed(rerr) || ctx.Err() != nil {
				return rerr
			}
			fmt.Fprintf(hc.Stderr, "bc: %v\n", rerr)
			return exitError{1}
		}
	}

	return nil
}
