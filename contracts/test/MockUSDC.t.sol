// SPDX-License-Identifier: MIT
pragma solidity 0.8.37;

import {MockUSDC} from "../src/MockUSDC.sol";
import {IERC20Errors} from "@openzeppelin/contracts/interfaces/draft-IERC6093.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {Test} from "forge-std/Test.sol";

contract MockUSDCTest is Test {
    MockUSDC internal token;

    address internal alice = makeAddr("alice");
    address internal bob = makeAddr("bob");
    address internal stranger = makeAddr("stranger");

    function setUp() public {
        token = new MockUSDC();
    }

    // S1-T101 — Req: ADR-8, §2.1.5
    function test_T101_metadata() public view {
        assertEq(token.name(), "Mock USDC");
        assertEq(token.symbol(), "USDC");
        assertEq(token.decimals(), 6);
    }

    // S1-T102 — Req: ADR-8, EVM §2.1.1
    function test_T102_mintByAnyone() public {
        uint256 amount = 25_400_000;

        vm.expectEmit(true, true, false, true, address(token));
        emit IERC20.Transfer(address(0), alice, amount);
        vm.prank(stranger);
        token.mint(alice, amount);

        assertEq(token.balanceOf(alice), amount);
        assertEq(token.balanceOf(stranger), 0);
        assertEq(token.totalSupply(), amount);
    }

    // S1-T103 — Req: EVM §2.1.1
    function test_T103_burnAndBurnFrom() public {
        uint256 minted = 100_000_000;
        uint256 burnt = 25_400_000;
        uint256 burntFrom = 10_000_000;
        token.mint(alice, minted);

        vm.expectEmit(true, true, false, true, address(token));
        emit IERC20.Transfer(alice, address(0), burnt);
        vm.prank(alice);
        token.burn(burnt);

        assertEq(token.balanceOf(alice), minted - burnt);
        assertEq(token.totalSupply(), minted - burnt);

        vm.prank(alice);
        token.approve(bob, burntFrom);

        vm.expectEmit(true, true, false, true, address(token));
        emit IERC20.Transfer(alice, address(0), burntFrom);
        vm.prank(bob);
        token.burnFrom(alice, burntFrom);

        assertEq(token.balanceOf(alice), minted - burnt - burntFrom);
        assertEq(token.totalSupply(), minted - burnt - burntFrom);
        assertEq(token.allowance(alice, bob), 0);

        vm.expectRevert(abi.encodeWithSelector(IERC20Errors.ERC20InsufficientAllowance.selector, stranger, 0, 1));
        vm.prank(stranger);
        token.burnFrom(alice, 1);
    }

    // S1-T104 — Req: EVM §2.1.1
    function test_T104_plainErc20() public {
        uint256 minted = 100_000_000;
        uint256 approved = 50_000_000;
        uint256 pulled = 25_400_000;
        uint256 sent = 10_000_000;
        token.mint(alice, minted);

        vm.prank(alice);
        assertTrue(token.approve(bob, approved));
        assertEq(token.allowance(alice, bob), approved);

        vm.prank(bob);
        assertTrue(token.transferFrom(alice, bob, pulled));
        assertEq(token.balanceOf(alice), minted - pulled);
        assertEq(token.balanceOf(bob), pulled);
        assertEq(token.allowance(alice, bob), approved - pulled);

        vm.prank(bob);
        assertTrue(token.transfer(alice, sent));
        assertEq(token.balanceOf(alice), minted - pulled + sent);
        assertEq(token.balanceOf(bob), pulled - sent);

        assertEq(token.totalSupply(), minted);
    }
}
