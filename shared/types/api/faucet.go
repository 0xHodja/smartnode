package api

import (
    "math/big"
)


type FaucetStatusResponse struct {
    Status string               `json:"status"`
    Error string                `json:"error"`
    Balance *big.Int            `json:"balance"`
    Allowance *big.Int          `json:"allowance"`
    WithdrawableAmount *big.Int `json:"withdrawableAmount"`
    ResetsInBlocks uint64       `json:"resetsInBlocks"`
}

