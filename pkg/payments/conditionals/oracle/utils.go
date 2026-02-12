package oracle

import (
	"crypto/ed25519"
	"errors"
	"math/big"
	"strings"
)

var (
	ErrEmptyPrice      = errors.New("empty price")
	ErrInvalidIntPart  = errors.New("invalid integer part")
	ErrInvalidFracPart = errors.New("invalid fractional part")
)

var binanceProofSignerSeed = []byte{
	0x71, 0x6e, 0x5b, 0x14, 0x2d, 0x37, 0x9a, 0x40,
	0x9b, 0x8f, 0xa8, 0x51, 0x55, 0xe5, 0x6b, 0x3f,
	0x03, 0x1f, 0xa4, 0x28, 0x59, 0xcc, 0x7b, 0x12,
	0x8a, 0xfd, 0x33, 0x72, 0x2e, 0x99, 0xb1, 0x64,
}

var binanceProofSignerKey = ed25519.NewKeyFromSeed(binanceProofSignerSeed)

func BinanceProofPublicKey() ed25519.PublicKey {
	return append(ed25519.PublicKey(nil), binanceProofSignerKey.Public().(ed25519.PublicKey)...)
}

// parsePriceToScaledInt converts a decimal price string (e.g., "43123.42") to an
// integer scaled by scale (e.g., 1e9). It truncates extra fractional digits without rounding.
func parsePriceToScaledInt(s string, scale int64) (*big.Int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, ErrEmptyPrice
	}
	neg := false
	if s[0] == '-' {
		neg = true
		s = s[1:]
	}
	parts := strings.SplitN(s, ".", 2)
	intPart := parts[0]
	fracPart := ""
	if len(parts) == 2 {
		fracPart = parts[1]
	}

	res := new(big.Int)
	if intPart != "" {
		ip, ok := new(big.Int).SetString(intPart, 10)
		if !ok {
			return nil, ErrInvalidIntPart
		}
		res.Set(ip)
	}
	res.Mul(res, big.NewInt(scale))

	if fracPart != "" {
		// keep up to scale digits
		scaleDigits := numDigits(scale) - 1 // since scale is power of 10
		if len(fracPart) > scaleDigits {
			fracPart = fracPart[:scaleDigits]
		}
		fp := new(big.Int)
		if fracPart != "" {
			v, ok := new(big.Int).SetString(fracPart, 10)
			if !ok {
				return nil, ErrInvalidFracPart
			}
			// multiply by 10^(scaleDigits - len(fracPart))
			pow := pow10(scaleDigits - len(fracPart))
			v.Mul(v, pow)
			fp.Set(v)
		}
		res.Add(res, fp)
	}
	if neg {
		res.Neg(res)
	}
	return res, nil
}

func numDigits(scale int64) int {
	// scale is expected to be a power of 10
	if scale <= 0 {
		return 0
	}
	c := 0
	for scale > 0 {
		scale /= 10
		c++
	}
	return c
}

func pow10(n int) *big.Int {
	if n <= 0 {
		return big.NewInt(1)
	}
	res := big.NewInt(1)
	ten := big.NewInt(10)
	for i := 0; i < n; i++ {
		res.Mul(res, ten)
	}
	return res
}
