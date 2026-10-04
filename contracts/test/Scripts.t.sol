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

    // S1-T704 — Req: Handoff §4
    function test_T704_chainGuard() public {
        Deploy deployScript = new Deploy();
        Scenario scenario = new Scenario();
        uint256[3] memory chainIds = [uint256(1), 8453, 84_532];

        for (uint256 i; i < chainIds.length; ++i) {
            vm.chainId(chainIds[i]);

            vm.expectRevert(bytes("Deploy: chain ID is not 31337 (local Anvil only)"));
            deployScript.run();

            vm.expectRevert(bytes("Deploy: chain ID is not 31337 (local Anvil only)"));
            deployScript.deploy(deployer, address(0), treasury, admin, operator);

            vm.expectRevert(bytes("Scenario: chain ID is not 31337 (local Anvil only)"));
            scenario.run();
        }
    }
}
