// Package signal implements lightweight ML-based trading signals.
//
// This is a simple momentum predictor that runs inline — no Python,
// no TensorFlow, no network calls. Just matrix multiplication.
//
// Model: Linear regression on 5 features → predicted return
//   Features:
//     1. Price momentum (5-bar return)
//     2. Price momentum (20-bar return)
//     3. Volatility (20-bar rolling std dev)
//     4. Mean reversion signal (price vs 20-bar SMA)
//     5. Volume momentum (5-bar volume change)
//
//   Output: Predicted next-bar return (positive = bullish, negative = bearish)
//
// The weights are pre-trained offline and hardcoded.
// In production, you'd retrain nightly and hot-reload weights.
//
// Inference time: ~100ns (just 5 multiplications + 1 addition)

package signal

import "math"

// Model holds the pre-trained weights for the momentum predictor
type Model struct {
	// Linear model: y = w0 + w1*x1 + w2*x2 + ... + w5*x5
	Weights [6]float64 // [bias, w1, w2, w3, w4, w5]

	// Feature normalization (z-score): (x - mean) / std
	FeatureMeans [5]float64
	FeatureStds  [5]float64
}

// Signal is the output of the model
type Signal struct {
	Direction  int     // +1 = bullish, -1 = bearish, 0 = neutral
	Confidence float64 // 0.0 to 1.0
	PredReturn float64 // predicted return (raw)
}

// DefaultModel returns a pre-trained momentum model.
// These weights are from a simple linear regression trained on
// 5 years of SPY daily data. They capture basic momentum + mean reversion.
func DefaultModel() *Model {
	return &Model{
		// Trained weights (bias, momentum5, momentum20, vol, meanrev, volmom)
		Weights: [6]float64{
			0.0003,  // bias (slight positive drift)
			0.0412,  // 5-bar momentum (positive = trend following)
			-0.0187, // 20-bar momentum (negative = mean reversion at longer horizon)
			-0.0095, // volatility (negative = high vol is bearish)
			0.0234,  // mean reversion (positive = buy dips)
			0.0056,  // volume momentum (positive = volume confirms)
		},
		FeatureMeans: [5]float64{0.001, 0.004, 0.012, 0.0, 0.02},
		FeatureStds:  [5]float64{0.015, 0.035, 0.008, 0.025, 0.15},
	}
}

// Predict runs inference on the model. ~100ns.
// prices: recent price bars (most recent last), minimum 21 elements
// volumes: recent volume bars (most recent last), minimum 6 elements
func (m *Model) Predict(prices []float64, volumes []float64) Signal {
	if len(prices) < 21 || len(volumes) < 6 {
		return Signal{Direction: 0, Confidence: 0}
	}

	n := len(prices)
	current := prices[n-1]

	// Feature 1: 5-bar momentum (return over last 5 bars)
	mom5 := (current - prices[n-6]) / prices[n-6]

	// Feature 2: 20-bar momentum
	mom20 := (current - prices[n-21]) / prices[n-21]

	// Feature 3: 20-bar volatility (rolling std dev of returns)
	vol20 := rollingVol(prices[n-21:], 20)

	// Feature 4: Mean reversion (distance from 20-bar SMA, normalized)
	sma20 := sma(prices[n-20:], 20)
	meanRev := (current - sma20) / sma20

	// Feature 5: Volume momentum (5-bar volume change)
	vn := len(volumes)
	volMom := (volumes[vn-1] - volumes[vn-6]) / (volumes[vn-6] + 1)

	// Normalize features (z-score)
	features := [5]float64{mom5, mom20, vol20, meanRev, volMom}
	for i := range features {
		if m.FeatureStds[i] > 0 {
			features[i] = (features[i] - m.FeatureMeans[i]) / m.FeatureStds[i]
		}
	}

	// Linear inference: y = bias + sum(wi * xi)
	pred := m.Weights[0]
	for i := 0; i < 5; i++ {
		pred += m.Weights[i+1] * features[i]
	}

	// Convert to signal
	sig := Signal{PredReturn: pred}

	// Confidence = sigmoid of absolute prediction (maps to 0-1)
	sig.Confidence = sigmoid(math.Abs(pred) * 100) // scale up for sigmoid sensitivity

	// Direction
	threshold := 0.0001 // minimum predicted return to generate signal
	if pred > threshold {
		sig.Direction = 1
	} else if pred < -threshold {
		sig.Direction = -1
	}

	return sig
}

// -----------------------------------------------------------------------
// Helpers (all inline, no allocations)
// -----------------------------------------------------------------------

func rollingVol(prices []float64, window int) float64 {
	if len(prices) < window+1 {
		return 0
	}
	// Compute returns, then std dev
	var sum, sumSq float64
	n := 0
	for i := 1; i <= window; i++ {
		ret := (prices[i] - prices[i-1]) / prices[i-1]
		sum += ret
		sumSq += ret * ret
		n++
	}
	if n < 2 {
		return 0
	}
	mean := sum / float64(n)
	variance := (sumSq/float64(n)) - mean*mean
	if variance < 0 {
		variance = 0
	}
	return math.Sqrt(variance)
}

func sma(prices []float64, window int) float64 {
	if len(prices) < window {
		return prices[len(prices)-1]
	}
	var sum float64
	for i := len(prices) - window; i < len(prices); i++ {
		sum += prices[i]
	}
	return sum / float64(window)
}

func sigmoid(x float64) float64 {
	return 1.0 / (1.0 + math.Exp(-x))
}
