// SPDX-License-Identifier: MIT
pragma solidity 0.8.37;

import {Deploy} from "../script/Deploy.s.sol";
import {Scenario} from "../script/Scenario.s.sol";
import {CardSpendController} from "../src/CardSpendController.sol";
import {MockUSDC} from "../src/MockUSDC.sol";
import {CardSpendControllerBase} from "./CardSpendController.Base.t.sol";

// Phase 7 of docs/test-plan-s1.md: the scripts run in-process; broadcast only switches the sender here.
// forge runs the tests of a contract in parallel and vm.setEnv is process-wide, so only T702 writes the
// script environment; T701 calls Deploy.deploy directly and T704 reverts before any env read.
contract ScriptsTest is CardSpendControllerBase {
    address internal deployer = makeAddr("deployer");

    function assertDeployed(address token_, address controller_) internal view {
        CardSpendController deployed = CardSpendController(controller_);
        assertEq(address(deployed.token()), token_);
        assertEq(deployed.treasury(), treasury);
        assertTrue(deployed.hasRole(deployed.DEFAULT_ADMIN_ROLE(), admin));
        assertTrue(deployed.hasRole(deployed.OPERATOR_ROLE(), operator));
    }

    // S1-T701 — Req: Package 7.1, handoff §4
    function test_T701_deployScript() public {
        vm.chainId(31_337);
        Deploy script = new Deploy();

        (address deployedToken, address deployedController) =
            script.deploy(deployer, address(0), treasury, admin, operator);
        assertEq(deployedToken, vm.computeCreateAddress(deployer, 0), "MockUSDC is deployed first");
        assertEq(deployedController, vm.computeCreateAddress(deployer, 1), "controller is deployed second");
        assertEq(MockUSDC(deployedToken).symbol(), "USDC");
        assertDeployed(deployedToken, deployedController);

        // With a token given, no MockUSDC is deployed.
        (address givenToken, address secondController) =
            script.deploy(deployer, address(token), treasury, admin, operator);
        assertEq(givenToken, address(token));
        assertEq(secondController, vm.computeCreateAddress(deployer, 2));
        assertDeployed(address(token), secondController);
    }

    // S1-T702 — Req: Package 7.2
    function test_T702_scenario() public {
        vm.chainId(31_337);
        vm.setEnv("DEPLOYER", vm.toString(deployer));
        vm.setEnv("TOKEN", vm.toString(address(0)));
        vm.setEnv("TREASURY", vm.toString(treasury));
        vm.setEnv("ADMIN", vm.toString(admin));
        vm.setEnv("OPERATOR", vm.toString(operator));
        vm.setEnv("USER", vm.toString(alice));

        (address deployedToken, address deployedController) = new Deploy().run();
        assertDeployed(deployedToken, deployedController);

        vm.setEnv("TOKEN", vm.toString(deployedToken));
        vm.setEnv("CONTROLLER", vm.toString(deployedController));
        // Reverts if any check of the scenario fails.
        new Scenario().run();

        assertEq(MockUSDC(deployedToken).balanceOf(deployedController), 0);
    }

    string internal constant DEPLOY_GUARD = "Deploy: chain ID is not 31337 (Anvil) or 84532 (Base Sepolia)";
    string internal constant SCENARIO_GUARD = "Scenario: chain ID is not 31337 (Anvil) or 84532 (Base Sepolia)";

    // S1-T704 — Req: Handoff §4. Since S2 (owner's decision D-3) 84532 is accepted: S2-T801.
    function test_T704_chainGuard() public {
        Deploy deployScript = new Deploy();
        Scenario scenario = new Scenario();
        uint256[3] memory chainIds = [uint256(1), 8453, 11_155_111];

        for (uint256 i; i < chainIds.length; ++i) {
            vm.chainId(chainIds[i]);

            vm.expectRevert(bytes(DEPLOY_GUARD));
            deployScript.run();

            vm.expectRevert(bytes(DEPLOY_GUARD));
            deployScript.deploy(deployer, address(0), treasury, admin, operator);

            vm.expectRevert(bytes(SCENARIO_GUARD));
            scenario.run();
        }
    }

    // The guard accepts chainId when the script gets past it: Deploy.deploy needs no environment; Scenario.run
    // reads the environment right after the guard, so any revert other than the guard's own is a pass.
    function assertGuardAccepts(uint256 chainId) internal {
        Deploy deployScript = new Deploy();
        Scenario scenario = new Scenario();
        vm.chainId(chainId);
        (address deployedToken, address deployedController) =
            deployScript.deploy(deployer, address(0), treasury, admin, operator);
        assertDeployed(deployedToken, deployedController);

        try scenario.run() {}
        catch Error(string memory reason) {
            assertNotEq(reason, SCENARIO_GUARD, "Scenario refuses the chain");
        } catch {}
    }

    // S2-T801 — Req: S1 summary §6, package 1.6, owner's decision D-3
    function test_S2_T801_acceptsAnvil() public {
        assertGuardAccepts(31_337);
    }

    // S2-T801 — Req: S1 summary §6, package 1.6, owner's decision D-3
    function test_S2_T801_acceptsBaseSepolia() public {
        assertGuardAccepts(84_532);
    }

    // S2-T801 — Req: S1 summary §6, package 1.6, owner's decision D-3
    function test_S2_T801_rejectsMainnet() public {
        Deploy deployScript = new Deploy();
        Scenario scenario = new Scenario();
        vm.chainId(1);

        vm.expectRevert(bytes(DEPLOY_GUARD));
        deployScript.run();

        vm.expectRevert(bytes(DEPLOY_GUARD));
        deployScript.deploy(deployer, address(0), treasury, admin, operator);

        vm.expectRevert(bytes(SCENARIO_GUARD));
        scenario.run();
    }
}
