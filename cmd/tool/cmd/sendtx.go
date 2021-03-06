/**
*  @file
*  @copyright defined in go-seele/LICENSE
 */

package cmd

import (
	"crypto/ecdsa"
	"fmt"
	"io/ioutil"
	"math/big"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/seeleteam/go-seele/cmd/util"
	"github.com/seeleteam/go-seele/common"
	"github.com/seeleteam/go-seele/crypto"
	"github.com/seeleteam/go-seele/rpc"
	"github.com/spf13/cobra"
	"github.com/seeleteam/go-seele/api"
)

var tps int
var debug bool

// send tx mode
// mode 1: send tx and check the txs periodically. add them back to balances after confirmed
// mode 2: send tx with amount 1 and don't care about new balances
// mode 3: split tx to 3 parts. send tx with full amount and replace old balances with new balances
var mode int

var wg = sync.WaitGroup{}

type balance struct {
	address    *common.Address
	privateKey *ecdsa.PrivateKey
	amount     int
	shard      uint
	nonce      uint64
	tx         *common.Hash
	packed     bool
}

var sendTxCmd = &cobra.Command{
	Use:   "sendtx",
	Short: "send tx peroidly",
	Long: `For example:
	tool.exe sendtx`,
	Run: func(cmd *cobra.Command, args []string) {
		initClient()
		balanceList := initAccount(threads)

		fmt.Println("use mode ", mode)
		fmt.Println("threads", threads)
		fmt.Println("total balance ", len(balanceList))
		balances := newBalancesList(balanceList, threads, true)

		for i := 0; i < threads; i++ {
			go StartSend(balances[i], i)
		}

		time.Sleep(5 * time.Second)
		wg.Wait()
	},
}

func StartSend(balanceList []*balance, threadNum int) {
	lock := &sync.Mutex{}
	if mode == 3 {
		wg.Add(1)
		loopSendMode3(balanceList)
	} else {
		wg.Add(1)
		go loopSendMode1_2(balanceList, lock, threadNum)
	}

	if mode == 1 {
		wg.Add(1)
		go loopCheckMode1(balanceList, lock)
	}
}

var tpsStartTime time.Time
var tpsCount = 0

func loopSendMode3(balanceList []*balance) {
	defer wg.Done()

	balances := newBalancesList(balanceList, 3, false)
	nextBalances := newBalancesList(balanceList, 3, true)

	tpsStartTime = time.Now()
	// send tx periodically
	for {
		SendMode3(balances[0], nextBalances[0])
		SendMode3(balances[1], nextBalances[1])
		SendMode3(balances[2], nextBalances[2])
	}
}

func newBalancesList(balanceList []*balance, splitNum int, copyValue bool) [][]*balance {
	balances := make([][]*balance, splitNum)
	unit := len(balanceList) / splitNum

	for i := 0; i < splitNum; i++ {
		var start = unit * i
		var end = unit * (i + 1)
		if i == splitNum-1 {
			end = len(balanceList)
		}

		balances[i] = make([]*balance, end-start)

		if copyValue {
			fmt.Printf("balance %d length %d\n", i, end-start)
			copy(balances[i], balanceList[start:end])
		}
	}

	return balances
}

func SendMode3(current []*balance, next []*balance) {
	copy(current, next)
	for i, b := range current {
		newBalance := send(b)
		if debug {
			fmt.Printf("send tx %s, account %s, nonce %d\n", newBalance.tx.ToHex(), b.address.ToHex(), b.nonce-1)
		}

		next[i] = newBalance

		tpsCount++
		if tpsCount == tps {
			fmt.Printf("send txs %d, [%d]\n", tpsCount, i)
			elapse := time.Now().Sub(tpsStartTime)
			if elapse < time.Second {
				time.Sleep(time.Second - elapse)
			}

			tpsCount = 0
			tpsStartTime = time.Now()
		}
	}
}

var txCh = make(chan *balance, 100000)

func loopSendMode1_2(balanceList []*balance, lock *sync.Mutex, threadNum int) {
	defer wg.Done()

	count := 0
	tpsStartTime = time.Now()

	// send tx periodically
	for {
		lock.Lock()
		copyBalances := make([]*balance, len(balanceList))
		copy(copyBalances, balanceList)
		fmt.Printf("balance total length %d at thread %d\n", len(balanceList), threadNum)
		lock.Unlock()

		for _, b := range copyBalances {
			newBalance := send(b)
			if mode == 1 {
				if newBalance.amount > 0 {
					txCh <- newBalance
				}
			}

			count++
			if count == tps {
				fmt.Printf("send txs %d at thread %d\n", count, threadNum)
				elapse := time.Now().Sub(tpsStartTime)
				if elapse < time.Second {
					time.Sleep(time.Second - elapse)
				}

				count = 0
				tpsStartTime = time.Now()
			}
		}

		lock.Lock()
		nextBalanceList := make([]*balance, 0)
		for _, b := range balanceList {
			if b.amount > 0 {
				nextBalanceList = append(nextBalanceList, b)
			}
		}
		balanceList = nextBalanceList
		lock.Unlock()
	}
}

func loopCheckMode1(balanceList []*balance, lock *sync.Mutex) {
	defer wg.Done()
	toPackedBalanceList := make([]*balance, 0)
	toConfirmBalanceList := make(map[time.Time][]*balance)

	var confirmTime = 2 * time.Minute
	checkPack := time.NewTicker(30 * time.Second)
	confirm := time.NewTicker(30 * time.Second)
	for {
		select {
		case b := <-txCh:
			toPackedBalanceList = append(toPackedBalanceList, b)
		case <-checkPack.C:
			included, pending := getIncludedAndPendingBalance(toPackedBalanceList)
			toPackedBalanceList = pending

			fmt.Printf("to packed balance: %d, new: %d\n", len(toPackedBalanceList), len(pending))
			toConfirmBalanceList[time.Now()] = included
			toPackedBalanceList = pending
		case <-confirm.C:
			for key, value := range toConfirmBalanceList {
				duration := time.Now().Sub(key)
				if duration > confirmTime {

					lock.Lock()
					balanceList = append(balanceList, value...)
					fmt.Printf("add confirmed balance %d, new: %d\n", len(value), len(balanceList))
					lock.Unlock()

					delete(toConfirmBalanceList, key)
				}
			}
		}
	}
}

func getIncludedAndPendingBalance(balances []*balance) ([]*balance, []*balance) {
	include := make([]*balance, 0)
	pending := make([]*balance, 0)
	for _, b := range balances {
		if b.tx == nil {
			continue
		}

		result := getTx(*b.address, *b.tx)
		if len(result) > 0 {
			if result["status"] == "block" {
				include = append(include, b)
			} else if result["status"] == "pool" {
				pending = append(pending, b)
			}

			if debug {
				fmt.Printf("got tx success %s from %s nonce %.0f status %s amount %.0f\n", b.tx.ToHex(), result["from"],
					result["accountNonce"], result["status"], result["amount"])
			}
		}
	}

	return include, pending
}

func getTx(address common.Address, hash common.Hash) map[string]interface{} {
	client := getClient(address)

	result, err := util.GetTransactionByHash(client, hash.ToHex())
	if err != nil {
		fmt.Println("failed to get tx ", err, " tx hash ", hash.ToHex())
		return result
	}

	return result
}

func send(b *balance) *balance {
	var amount = 1
	if mode == 1 {
		amount = rand.Intn(b.amount) // for test, amount will always keep in int value.
	} else if mode == 3 {
		amount = b.amount
	}

	addr, privateKey := crypto.MustGenerateShardKeyPair(b.address.Shard())
	newBalance := &balance{
		address:    addr,
		privateKey: privateKey,
		amount:     amount,
		shard:      addr.Shard(),
		nonce:      0,
		packed:     false,
	}

	value := big.NewInt(int64(amount))
	value.Mul(value, common.SeeleToFan)

	client := getRandClient()
	tx, err := util.GenerateTx(b.privateKey, *addr, value, big.NewInt(1), b.nonce, nil)
	if err != nil {
		return newBalance
	}

	ok, err := util.SendTx(client, tx)
	if !ok || err != nil {
		return newBalance
	}

	// update balance by transaction amount and update nonce
	b.nonce++
	b.amount -= amount
	newBalance.tx = &tx.Hash

	return newBalance
}

func getRandClient() *rpc.Client {
	if len(clientList) == 0 {
		panic("no client found")
	}

	index := rand.Intn(len(clientList))

	count := 0
	for _, v := range clientList {
		if count == index {
			return v
		}

		count++
	}

	return nil
}

func initAccount(threads int) []*balance {
	keys, err := ioutil.ReadFile(keyFile)
	if err != nil {
		panic(fmt.Sprintf("failed to read key file %s", err))
	}

	keyList := strings.Split(string(keys), "\r\n")
	unit := len(keyList) / threads

	wg := &sync.WaitGroup{}
	balanceList := make([]*balance, len(keyList))
	for i := 0; i < threads; i++ {
		end := (i + 1) * unit
		if i == threads-1 {
			end = len(keyList)
		}

		wg.Add(1)
		go initBalance(balanceList, keyList, i*unit, end, wg)
	}

	wg.Wait()

	result := make([]*balance, 0)
	for _, b := range balanceList {
		if b != nil && b.amount > 0 {
			result = append(result, b)
		}
	}

	return result
}

func initBalance(balanceList []*balance, keyList []string, start int, end int, wg *sync.WaitGroup) {
	defer wg.Done()

	// init balance and nonce
	for i := start; i < end; i++ {
		hex := keyList[i]
		if hex == "" {
			continue
		}

		key, err := crypto.LoadECDSAFromString(hex)
		if err != nil {
			panic(fmt.Sprintf("failed to load key %s", err))
		}

		addr := crypto.GetAddress(&key.PublicKey)
		// skip address that don't find the same shard client
		if _, ok := clientList[addr.Shard()]; !ok {
			continue
		}

		amount, ok := getBalance(*addr)
		if !ok {
			continue
		}

		b := &balance{
			address:    addr,
			privateKey: key,
			amount:     amount,
			shard:      addr.Shard(),
			packed:     false,
		}

		fmt.Printf("%s balance is %d\n", b.address.ToHex(), b.amount)

		if b.amount > 0 {
			b.nonce = getNonce(*b.address)
			balanceList[i] = b
		}
	}
}

func getBalance(address common.Address) (int, bool) {
	client := getClient(address)

	var result api.GetBalanceResponse
	if err := client.Call(&result, "seele_getBalance", address); err != nil {
		panic(fmt.Sprintf("failed to get the balance: %s\n", err))
	}

	return int(result.Balance.Div(result.Balance, common.SeeleToFan).Uint64()), true
}

func getClient(address common.Address) *rpc.Client {
	shard := address.Shard()
	client := clientList[shard]
	if client == nil {
		panic(fmt.Sprintf("not found client in shard %d", shard))
	}

	return client
}

func getNonce(address common.Address) uint64 {
	client := getClient(address)

	nonce, err := util.GetAccountNonce(client, address)
	if err != nil {
		panic(err)
	}

	return nonce
}

func getShard(client *rpc.Client) uint {
	info, err := util.GetInfo(client)
	if err != nil {
		panic(fmt.Sprintf("failed to get the balance: %s\n", err.Error()))
	}

	return info.Coinbase.Shard() // @TODO need refine this code, get shard info straight
}

func init() {
	rootCmd.AddCommand(sendTxCmd)

	sendTxCmd.Flags().StringVarP(&keyFile, "keyfile", "f", "keystore.txt", "key store file")
	sendTxCmd.Flags().IntVarP(&tps, "tps", "", 3, "target tps to send transaction")
	sendTxCmd.Flags().BoolVarP(&debug, "debug", "d", false, "whether print more debug info")
	sendTxCmd.Flags().IntVarP(&mode, "mode", "m", 1, "send tx mode")
	sendTxCmd.Flags().IntVarP(&threads, "threads", "t", 1, "send tx threads")
}
