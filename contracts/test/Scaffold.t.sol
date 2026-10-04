// SPDX-License-Identifier: MIT
pragma solidity 0.8.37;

// Temporary scaffold test: it exists only so that `forge test` runs a test before the contracts exist.
// Delete in st3.

import {Test} from "forge-std/Test.sol";

contract ScaffoldTest is Test {
    function test_scaffold_compiles() public pure {
        assertTrue(true);
    }
}
