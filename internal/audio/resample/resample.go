// Package resample provides a small streaming band-limited sample rate
// converter. It is only used when the virtual audio device cannot be opened at
// the call rate; PulseAudio and PipeWire both resample natively, so the
// default configuration bypasses this code entirely.
package resample

import (
	"math"
	"sync"
)

const tableRes = 64

type ratePair struct{ in, out int }

var kernelCache sync.Map // ratePair -> *kernel

type kernel struct {
	halfWidth int
	table     []float64 // windowed sinc sampled at 1/tableRes input samples
	norm      []float64 // per fractional phase DC gain correction
}

func getKernel(in, out int) *kernel {
	key := ratePair{in, out}
	if v, ok := kernelCache.Load(key); ok {
		return v.(*kernel)
	}
	k := buildKernel(in, out)
	actual, _ := kernelCache.LoadOrStore(key, k)
	return actual.(*kernel)
}

func buildKernel(in, out int) *kernel {
	ratio := float64(out) / float64(in)
	hw := 24
	if ratio < 1 {
		// Anti-alias filters need a longer response when decimating.
		hw = 64
	}
	cutoff := 0.92 * math.Min(1, ratio)
	const beta = 8.5

	n := 2*hw*tableRes + 1
	table := make([]float64, n)
	for i := 0; i < n; i++ {
		x := float64(i)/tableRes - float64(hw)
		table[i] = cutoff * sinc(cutoff*x) * kaiser(x, float64(hw), beta)
	}

	norm := make([]float64, tableRes+1)
	for ph := 0; ph <= tableRes; ph++ {
		frac := float64(ph) / tableRes
		var sum float64
		for k := -hw + 1; k <= hw; k++ {
			sum += tableValue(table, hw, frac-float64(k))
		}
		if sum == 0 {
			sum = 1
		}
		norm[ph] = 1 / sum
	}
	return &kernel{halfWidth: hw, table: table, norm: norm}
}

func sinc(x float64) float64 {
	if x == 0 {
		return 1
	}
	x *= math.Pi
	return math.Sin(x) / x
}

func kaiser(x, hw, beta float64) float64 {
	if x < -hw || x > hw {
		return 0
	}
	r := x / hw
	return besselI0(beta*math.Sqrt(1-r*r)) / besselI0(beta)
}

// besselI0 is the modified Bessel function of the first kind, order zero,
// using the polynomial approximation from Abramowitz and Stegun 9.8.1/9.8.2.
func besselI0(x float64) float64 {
	ax := math.Abs(x)
	if ax < 3.75 {
		y := (x / 3.75) * (x / 3.75)
		return 1 + y*(3.5156229+y*(3.0899424+y*(1.2067492+
			y*(0.2659732+y*(0.0360768+y*0.0045813)))))
	}
	y := 3.75 / ax
	return (math.Exp(ax) / math.Sqrt(ax)) * (0.39894228 + y*(0.01328592+
		y*(0.00225319+y*(-0.00157565+y*(0.00916281+
			y*(-0.02057706+y*(0.02635537+y*(-0.01647633+y*0.00392377))))))))
}

func tableValue(table []float64, hw int, x float64) float64 {
	f := (x + float64(hw)) * tableRes
	if f <= 0 {
		return table[0]
	}
	i := int(f)
	if i >= len(table)-1 {
		return table[len(table)-1]
	}
	t := f - float64(i)
	return table[i]*(1-t) + table[i+1]*t
}

// Resampler converts a mono int16 stream between two rates. It is not safe for
// concurrent use.
type Resampler struct {
	inRate      int
	outRate     int
	step        float64
	hw          int
	k           *kernel
	buf         []float64
	pos         float64
	passthrough bool
}

// New creates a resampler from inRate to outRate. Equal or invalid rates
// produce a passthrough converter.
func New(inRate, outRate int) *Resampler {
	r := &Resampler{inRate: inRate, outRate: outRate}
	if inRate <= 0 || outRate <= 0 || inRate == outRate {
		r.passthrough = true
		return r
	}
	r.k = getKernel(inRate, outRate)
	r.hw = r.k.halfWidth
	r.step = float64(inRate) / float64(outRate)
	// Prime the history with silence so the first output sample lines up with
	// the first input sample.
	r.buf = make([]float64, r.hw)
	r.pos = float64(r.hw)
	return r
}

// Rates reports the configured input and output rates.
func (r *Resampler) Rates() (int, int) { return r.inRate, r.outRate }

// Passthrough reports whether no conversion is performed.
func (r *Resampler) Passthrough() bool { return r.passthrough }

// Process converts one chunk. The returned slice is newly allocated; the input
// is not modified. When Passthrough is true the input slice is returned as is.
func (r *Resampler) Process(in []int16) []int16 {
	if r.passthrough {
		return in
	}
	for _, s := range in {
		r.buf = append(r.buf, float64(s))
	}

	out := make([]int16, 0, len(in)*r.outRate/r.inRate+4)
	hw := r.hw
	tbl := r.k.table
	norm := r.k.norm
	for {
		i0 := int(r.pos)
		if i0+hw >= len(r.buf) {
			break
		}
		frac := r.pos - float64(i0)
		var acc float64
		for k := -hw + 1; k <= hw; k++ {
			acc += r.buf[i0+k] * tableValue(tbl, hw, frac-float64(k))
		}
		nf := frac * tableRes
		ni := int(nf)
		if ni >= tableRes {
			ni = tableRes - 1
		}
		nt := nf - float64(ni)
		acc *= norm[ni]*(1-nt) + norm[ni+1]*nt
		out = append(out, clamp(acc))
		r.pos += r.step
	}

	if keep := int(r.pos) - hw + 1; keep > 0 {
		if keep > len(r.buf) {
			keep = len(r.buf)
		}
		r.buf = append(r.buf[:0], r.buf[keep:]...)
		r.pos -= float64(keep)
	}
	return out
}

// Reset clears the streaming state.
func (r *Resampler) Reset() {
	if r.passthrough {
		return
	}
	r.buf = r.buf[:0]
	for i := 0; i < r.hw; i++ {
		r.buf = append(r.buf, 0)
	}
	r.pos = float64(r.hw)
}

func clamp(v float64) int16 {
	if v > math.MaxInt16 {
		return math.MaxInt16
	}
	if v < math.MinInt16 {
		return math.MinInt16
	}
	return int16(math.Round(v))
}
