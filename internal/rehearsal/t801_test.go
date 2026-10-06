package rehearsal

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

var deployedLine = regexp.MustCompile(`(?m)^\s*(TOKEN|CONTROLLER)=(0x[0-9a-fA-F]{40})\s*$`)

// TestT801_DeployChainGuard runs contracts/script/Deploy.s.sol with broadcast on three local Anvils: chain ID 31337
// and 84532 deploy, chain ID 1 reverts with the guard and mines nothing. The scripts sign with Anvil's unlocked
// accounts (contracts/README.md); forge writes its records into a temporary directory.
func TestT801_DeployChainGuard(t *testing.T) {
	requireTools(t, "anvil", "forge")
	for _, c := range []struct {
		chainID uint64
		allowed bool
	}{{31337, true}, {1, false}, {baseSepolia, true}} {
		chain := startAnvil(t, c.chainID)
		var accounts []common.Address
		chain.Call(t, &accounts, "eth_accounts")
		cmd := exec.Command("forge", "script", "script/Deploy.s.sol", "--rpc-url", chain.RPCURL, "--broadcast",
			"--unlocked", "--sender", accounts[0].Hex())
		cmd.Dir = filepath.Join(repoRoot(), "contracts")
		cmd.Env = with(baseEnv(),
			"FOUNDRY_OFFLINE=true",
			"FOUNDRY_BROADCAST="+t.TempDir(),
			"DEPLOYER="+accounts[0].Hex(), "ADMIN="+accounts[1].Hex(), "OPERATOR="+accounts[2].Hex(), "TREASURY="+accounts[3].Hex(),
			"TOKEN="+common.Address{}.Hex(),
		)
		out, err := cmd.CombinedOutput()

		var block hexutil.Uint64
		chain.Call(t, &block, "eth_blockNumber")
		if !c.allowed {
			if err == nil || !strings.Contains(string(out), "Deploy: chain ID is not 31337 (Anvil) or 84532 (Base Sepolia)") {
				t.Errorf("chain ID %d: want the guard's revert, got err %v:\n%s", c.chainID, err, out)
			}
			if block != 0 {
				t.Errorf("chain ID %d: %d blocks mined, want none", c.chainID, block)
			}
			continue
		}
		if err != nil {
			t.Fatalf("chain ID %d: forge script: %v\n%s", c.chainID, err, out)
		}
		found := map[string]common.Address{}
		for _, m := range deployedLine.FindAllStringSubmatch(string(out), -1) {
			found[m[1]] = common.HexToAddress(m[2])
		}
		for _, name := range []string{"TOKEN", "CONTROLLER"} {
			var code hexutil.Bytes
			chain.Call(t, &code, "eth_getCode", found[name], "latest")
			if len(code) == 0 {
				t.Errorf("chain ID %d: no code at %s %s", c.chainID, name, found[name].Hex())
			}
		}
		t.Logf("chain ID %d: deployed in %d blocks", c.chainID, block)
	}
}
