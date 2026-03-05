package model

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"time"

	"gonum.org/v1/gonum/mat"
)

// ModelWeights stores W and B for Y = X @ W + B
type ModelWeights struct {
	W       []float64 `json:"w"`        // shape: [input_dim, 1]
	B       float64   `json:"b"`         // scalar bias
	InputDim int      `json:"input_dim"`
}

// Model is the inference runtime.
type Model struct {
	weights  *ModelWeights
	inputDim int
}

const weightsFile = "server/model.weights"

// Load loads weights from disk, generating them if they don't exist.
func Load(inputDim int) (*Model, error) {
	if err := os.MkdirAll("server", 0755); err != nil {
		return nil, fmt.Errorf("create server dir: %w", err)
	}

	var weights *ModelWeights

	data, err := os.ReadFile(weightsFile)
	if err != nil {
		// Generate weights
		fmt.Printf("[Model] Generating new weights (input_dim=%d)...\n", inputDim)
		weights = generate(inputDim)
		b, err := json.Marshal(weights)
		if err != nil {
			return nil, fmt.Errorf("marshal weights: %w", err)
		}
		if err := os.WriteFile(weightsFile, b, 0644); err != nil {
			return nil, fmt.Errorf("save weights: %w", err)
		}
		fmt.Printf("[Model] Weights saved to %s\n", weightsFile)
	} else {
		weights = &ModelWeights{}
		if err := json.Unmarshal(data, weights); err != nil {
			return nil, fmt.Errorf("unmarshal weights: %w", err)
		}
		if weights.InputDim != inputDim {
			fmt.Printf("[Model] Dim mismatch (file=%d, env=%d), regenerating\n", weights.InputDim, inputDim)
			weights = generate(inputDim)
			b, _ := json.Marshal(weights)
			os.WriteFile(weightsFile, b, 0644)
		}
		fmt.Printf("[Model] Loaded weights from %s (input_dim=%d)\n", weightsFile, weights.InputDim)
	}

	return &Model{weights: weights, inputDim: inputDim}, nil
}

// generate creates random weights using Xavier initialization.
func generate(inputDim int) *ModelWeights {
	scale := 1.0 / float64(inputDim)
	w := make([]float64, inputDim)
	for i := range w {
		w[i] = (rand.Float64()*2 - 1) * scale
	}
	return &ModelWeights{
		W:        w,
		B:        rand.Float64()*0.1 - 0.05,
		InputDim: inputDim,
	}
}

// RunInference runs Y = X @ W + B and returns (outputs, latency_ms).
// input shape: [batch_size][input_dim]
// output shape: [batch_size]
func (m *Model) RunInference(input [][]float64) ([]float64, float64, error) {
	if len(input) == 0 {
		return nil, 0, fmt.Errorf("empty input batch")
	}
	for i, row := range input {
		if len(row) != m.inputDim {
			return nil, 0, fmt.Errorf("row %d: expected %d dims, got %d", i, m.inputDim, len(row))
		}
	}

	start := time.Now()

	batchSize := len(input)

	// Flatten input to matrix data
	flatX := make([]float64, batchSize*m.inputDim)
	for i, row := range input {
		copy(flatX[i*m.inputDim:(i+1)*m.inputDim], row)
	}

	X := mat.NewDense(batchSize, m.inputDim, flatX)
	W := mat.NewDense(m.inputDim, 1, m.weights.W)

	// Y = X @ W
	Y := mat.NewDense(batchSize, 1, nil)
	Y.Mul(X, W)

	// + B
	output := make([]float64, batchSize)
	for i := 0; i < batchSize; i++ {
		output[i] = Y.At(i, 0) + m.weights.B
	}

	latencyMs := float64(time.Since(start).Microseconds()) / 1000.0

	return output, latencyMs, nil
}

// InputDim returns the expected input dimension.
func (m *Model) InputDim() int {
	return m.inputDim
}
