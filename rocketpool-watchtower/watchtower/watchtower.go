package watchtower

import (
    "encoding/json"
    "errors"
    "fmt"
    "math/big"
    "sync"
    "time"

    "github.com/ethereum/go-ethereum/common"

    "github.com/rocket-pool/smartnode/shared/services"
    beaconchain "github.com/rocket-pool/smartnode/shared/services/beacon-chain"
    "github.com/rocket-pool/smartnode/shared/services/rocketpool/minipool"
    "github.com/rocket-pool/smartnode/shared/utils/eth"
)


// Config
const CHECK_TRUSTED_INTERVAL string = "1m"
const DEFAULT_CHECK_MINIPOOLS_INTERVAL string = "1m"
var checkTrustedInterval, _ = time.ParseDuration(CHECK_TRUSTED_INTERVAL)


// Watchtower process
type WatchtowerProcess struct {
    p                    *services.Provider
    updatingMinipools    bool
    stopCheckMinipools   chan struct{}
    beaconMessageChannel chan interface{}
    activeMinipools      map[string]common.Address
    txLock               sync.Mutex
}


/**
 * Start beacon watchtower process
 */
func StartWatchtowerProcess(p *services.Provider) {

    // Initialise process
    process := &WatchtowerProcess{
        p:                    p,
        updatingMinipools:    false,
        stopCheckMinipools:   make(chan struct{}),
        beaconMessageChannel: make(chan interface{}),
        activeMinipools:      make(map[string]common.Address),
    }

    // Start
    process.start()

}


/**
 * Start process
 */
func (p *WatchtowerProcess) start() {

    // Check if node is trusted on interval
    go (func() {
        p.checkTrusted()
        checkTrustedTimer := time.NewTicker(checkTrustedInterval)
        for _ = range checkTrustedTimer.C {
            p.checkTrusted()
        }
    })()

    // Handle beacon chain events
    go (func() {
        for eventData := range p.beaconMessageChannel {
            event := (eventData).(struct{Client *beaconchain.Client; Message []byte})
            p.onBeaconClientMessage(event.Message)
        }
    })()

}


/**
 * Check if node is trusted
 */
func (p *WatchtowerProcess) checkTrusted() {

    // Wait for node to sync
    eth.WaitSync(p.p.Client, true, false)

    // Get trusted status
    nodeAccount, _ := p.p.AM.GetNodeAccount()
    trusted := new(bool)
    if err := p.p.CM.Contracts["rocketNodeAPI"].Call(nil, trusted, "getTrusted", nodeAccount.Address); err != nil {
        p.p.Log.Println(errors.New("Error retrieving node trusted status: " + err.Error()))
        return
    }

    // Start/stop minipool updates
    if *trusted {
        p.startUpdateMinipools()
    } else {
        p.stopUpdateMinipools()
    }

}


/**
 * Start minipool updates
 */
func (p *WatchtowerProcess) startUpdateMinipools() {

    // Cancel if already updating minipools
    if p.updatingMinipools { return }
    p.updatingMinipools = true

    // Log
    p.p.Log.Println("Node is trusted, starting watchtower process...")

    // Check minipools
    p.checkMinipools()

    // Subscribe to beacon chain events
    p.p.Publisher.AddSubscriber("beacon.client.message", p.beaconMessageChannel)

}


/**
 * Stop minipool updates
 */
func (p *WatchtowerProcess) stopUpdateMinipools() {

    // Cancel if not updating minipools
    if !p.updatingMinipools { return }
    p.updatingMinipools = false

    // Log
    p.p.Log.Println("Node is untrusted, stopping watchtower process...")

    // Stop checking minipools
    p.stopCheckMinipools <- struct{}{}

    // Unsubscribe from beacon chain events
    p.p.Publisher.RemoveSubscriber("beacon.client.message", p.beaconMessageChannel)

}


/**
 * Check active minipools
 */
func (p *WatchtowerProcess) checkMinipools() {

    // Log
    p.p.Log.Println("Checking active minipools...")

    // Wait for node to sync
    eth.WaitSync(p.p.Client, true, false)

    // Get active minipools
    if activeMinipools, err := minipool.GetActiveMinipoolsByValidatorPubkey(p.p.CM); err != nil {
        p.p.Log.Println(errors.New("Error getting active minipools: " + err.Error()))
        return
    } else {
        p.activeMinipools = *activeMinipools
    }

    // Request validator statuses for active minipools
    for pubkey, minipoolAddress := range p.activeMinipools {
        go (func(pubkey string, minipoolAddress common.Address) {

            // Log
            p.p.Log.Println(fmt.Sprintf("Checking minipool %s status...", minipoolAddress.Hex()))

            // Request validator status
            if payload, err := json.Marshal(beaconchain.ClientMessage{
                Message: "get_validator_status",
                Pubkey:  pubkey,
            }); err != nil {
                p.p.Log.Println(errors.New("Error encoding get validator status payload: " + err.Error()))
            } else if err := p.p.Beacon.Send(payload); err != nil {
                p.p.Log.Println(errors.New("Error sending get validator status message: " + err.Error()))
            }

        })(pubkey, minipoolAddress)
    }

    // Schedule next minipool check
    p.scheduleCheckMinipools()

}


/**
 * Schedule next minipool check
 */
func (p *WatchtowerProcess) scheduleCheckMinipools() {

    // Wait for node to sync
    eth.WaitSync(p.p.Client, true, false)

    // Get current check interval
    var checkInterval time.Duration
    checkIntervalSeconds := new(*big.Int)
    if err := p.p.CM.Contracts["rocketMinipoolSettings"].Call(nil, checkIntervalSeconds, "getMinipoolCheckInterval"); err == nil {
        checkInterval, _ = time.ParseDuration((*checkIntervalSeconds).String() + "s")
    }
    if checkInterval.Seconds() == 0 {
        checkInterval, _ = time.ParseDuration(DEFAULT_CHECK_MINIPOOLS_INTERVAL)
    }

    // Log check interval
    p.p.Log.Println("Time until next minipool check:", checkInterval.String())

    // Initialise check timer
    go (func() {
        checkMinipoolsTimer := time.NewTimer(checkInterval)
        select {
            case <-checkMinipoolsTimer.C:
                p.checkMinipools()
            case <-p.stopCheckMinipools:
                checkMinipoolsTimer.Stop()
        }
    })()

}


/**
 * Handle beacon chain client messages
 */
func (p *WatchtowerProcess) onBeaconClientMessage(messageData []byte) {

    // Lock for transactions
    p.txLock.Lock()
    defer p.txLock.Unlock()

    // Parse message
    message := new(beaconchain.ServerMessage)
    if err := json.Unmarshal(messageData, message); err != nil {
        p.p.Log.Println(errors.New("Error decoding beacon message: " + err.Error()))
        return
    }

    // Handle exit and withdrawal validator status messages only
    if message.Message != "validator_status" { return }
    if !(message.Status.Code == "exited" || message.Status.Code == "withdrawable") { return }

    // Get associated minipool
    minipoolAddress, ok := p.activeMinipools[message.Pubkey]
    if !ok { return }

    // Wait for node to sync
    eth.WaitSync(p.p.Client, true, false)

    // Get minipool status
    status, err := getMinipoolStatus(p.p, &minipoolAddress)
    if err != nil {
        p.p.Log.Println(err)
        return
    }

    // Log minipool out if staking
    if *status == minipool.STAKING {

        // Log
        p.p.Log.Println(fmt.Sprintf("Minipool %s is ready for logout...", minipoolAddress.Hex()))

        // Log out
        if txor, err := p.p.AM.GetNodeAccountTransactor(); err != nil {
            p.p.Log.Println(err)
        } else {
            if _, err := eth.ExecuteContractTransaction(p.p.Client, txor, p.p.CM.Addresses["rocketNodeWatchtower"], p.p.CM.Abis["rocketNodeWatchtower"], "logoutMinipool", minipoolAddress); err != nil {
                p.p.Log.Println(errors.New("Error logging out minipool: " + err.Error()))
            } else {
                p.p.Log.Println(fmt.Sprintf("Minipool %s was successfully logged out", minipoolAddress.Hex()))
            }
        }

        return
    }

    // Withdraw minipool if logged out and withdrawable
    if *status == minipool.LOGGED_OUT && message.Status.Code == "withdrawable" {

        // Log
        p.p.Log.Println(fmt.Sprintf("Minipool %s is ready for withdrawal...", minipoolAddress.Hex()))

        // Get balance to withdraw
        balanceWei := eth.GweiToWei(float64(message.Balance))

        // Withdraw
        if txor, err := p.p.AM.GetNodeAccountTransactor(); err != nil {
            p.p.Log.Println(err)
        } else {
            if _, err := eth.ExecuteContractTransaction(p.p.Client, txor, p.p.CM.Addresses["rocketNodeWatchtower"], p.p.CM.Abis["rocketNodeWatchtower"], "withdrawMinipool", minipoolAddress, balanceWei); err != nil {
                p.p.Log.Println(errors.New("Error withdrawing minipool: " + err.Error()))
            } else {
                p.p.Log.Println(fmt.Sprintf("Minipool %s was successfully withdrawn with a balance of %.2f ETH", minipoolAddress.Hex(), eth.WeiToEth(balanceWei)))
            }
        }

        return
    }

}


// Get a minipool's status
func getMinipoolStatus(p *services.Provider, address *common.Address) (*uint8, error) {

    // Initialise minipool contract
    minipoolContract, err := p.CM.NewContract(address, "rocketMinipool")
    if err != nil {
        return nil, errors.New("Error initialising minipool contract: " + err.Error())
    }

    // Get minipool's current status
    status := new(uint8)
    if err := minipoolContract.Call(nil, status, "getStatus"); err != nil {
        return nil, errors.New("Error retrieving minipool status: " + err.Error())
    }

    // Return
    return status, nil

}

