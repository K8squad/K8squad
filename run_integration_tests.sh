#!/bin/bash

# Comprehensive Integration Test Runner for Backup DevOps Engineer System
# This script runs all integration tests and provides detailed output

set -e

echo "=================================================="
echo "Backup DevOps Engineer Integration Test Suite"
echo "=================================================="
echo

# Check if we're in the correct directory
if [ ! -f "test_agent_integration.go" ]; then
    echo "ERROR: test_agent_integration.go not found in current directory"
    echo "Please run this script from the /mnt/nas/project/k8squad directory"
    exit 1
fi

# Check if Go is available
if ! command -v go &> /dev/null; then
    echo "ERROR: Go is not installed or not in PATH"
    echo "Please install Go 1.25.0 or later"
    exit 1
fi

# Check Go version
GO_VERSION=$(go version | cut -d' ' -f3 | cut -d'.' -f2)
if [ "$GO_VERSION" -lt 25 ]; then
    echo "ERROR: Go version 1.25.0 or later is required"
    echo "Current version: $(go version)"
    exit 1
fi

echo "Go version: $(go version)"
echo

# Function to run individual tests
run_test() {
    local test_name=$1
    echo "Running: $test_name"
    echo "----------------------------------------"
    
    if go test -v -run "$test_name"; then
        echo "✓ $test_name: PASSED"
    else
        echo "✗ $test_name: FAILED"
        return 1
    fi
    echo
}

# Function to test real execution capabilities
test_real_execution() {
    echo "Testing Real Execution Capabilities"
    echo "========================================"
    
    # Run tests that specifically verify real vs simulation
    echo "1. Testing real backup infrastructure operations..."
    if go test -v -run "TestRealBackupInfrastructureOperations" > /tmp/test_real_backup.log 2>&1; then
        echo "✓ Real backup operations: PASSED"
        grep -E "(Starting real execution|Real execution completed successfully|Backup completed successfully)" /tmp/test_real_backup.log | head -3
    else
        echo "✗ Real backup operations: FAILED"
        cat /tmp/test_real_backup.log | grep -E "(FAIL|error)" | head -3
    fi
    echo

    echo "2. Testing real execution vs simulation..."
    if go test -v -run "TestRealExecutionVsSimulation" > /tmp/test_real_vs_sim.log 2>&1; then
        echo "✓ Real vs simulation detection: PASSED"
        grep -E "(Starting real execution|Real execution completed successfully|Operation should take)" /tmp/test_real_vs_sim.log | head -3
    else
        echo "✗ Real vs simulation detection: FAILED"
        cat /tmp/test_real_vs_sim.log | grep -E "(FAIL|error)" | head -3
    fi
    echo
}

# Function to test error handling
test_error_handling() {
    echo "Testing Error Handling and Edge Cases"
    echo "========================================"
    
    if go test -v -run "TestErrorHandlingAndEdgeCases" > /tmp/test_error_handling.log 2>&1; then
        echo "✓ Error handling: PASSED"
        grep -E "(Unknown operation should fail|Missing parameters should fail|Non-existent agent should fail)" /tmp/test_error_handling.log | head -3
    else
        echo "✗ Error handling: FAILED"
        cat /tmp/test_error_handling.log | grep -E "(FAIL|error)" | head -5
    fi
    echo
}

# Function to test concurrent operations
test_concurrent_operations() {
    echo "Testing Concurrent Operations"
    echo "========================================"
    
    if go test -v -run "TestConcurrentOperations" > /tmp/test_concurrent.log 2>&1; then
        echo "✓ Concurrent operations: PASSED"
        grep -E "(Concurrent operations should complete successfully|All operations should complete successfully)" /tmp/test_concurrent.log | head -3
    else
        echo "✗ Concurrent operations: FAILED"
        cat /tmp/test_concurrent.log | grep -E "(FAIL|error)" | head -3
    fi
    echo
}

# Function to test state consistency
test_state_consistency() {
    echo "Testing State Consistency"
    echo "========================================"
    
    if go test -v -run "TestAgentStateConsistency" > /tmp/test_state.log 2>&1; then
        echo "✓ State consistency: PASSED"
        grep -E "(should be in completed state|Should have exactly 3 available agents)" /tmp/test_state.log | head -3
    else
        echo "✗ State consistency: FAILED"
        cat /tmp/test_state.log | grep -E "(FAIL|error)" | head -3
    fi
    echo
}

# Function to test logging
test_logging() {
    echo "Testing Comprehensive Logging"
    echo "========================================"
    
    if go test -v -run "TestComprehensiveLoggingVerification" > /tmp/test_logging.log 2>&1; then
        echo "✓ Comprehensive logging: PASSED"
        grep -E "(Log should contain expected marker|Log should contain current year timestamp)" /tmp/test_logging.log | head -3
    else
        echo "✗ Comprehensive logging: FAILED"
        cat /tmp/test_logging.log | grep -E "(FAIL|error)" | head -3
    fi
    echo
}

# Main test execution
echo "Starting comprehensive integration test suite..."
echo

# Test 1: Complete execution flow
echo "Phase 1: Complete Execution Flow"
echo "========================================"
run_test "TestCompleteAgentExecutionFlow"

# Test 2: Individual operation types
echo "Phase 2: Individual Operation Types"
echo "========================================"
run_test "TestRealBackupInfrastructureOperations"
run_test "TestDisasterRecoveryOperations"
run_test "TestConfigurationSyncOperations"

# Test 3: Silent runs verification
echo "Phase 3: Silent Runs Verification"
echo "========================================"
run_test "TestNoSilentActiveRuns"

# Test 4: Comprehensive logging
test_logging

# Test 5: Error handling
test_error_handling

# Test 6: Real execution verification
test_real_execution

# Test 7: Concurrent operations
test_concurrent_operations

# Test 8: State consistency
test_state_consistency

# Generate summary report
echo "=================================================="
echo "Integration Test Summary"
echo "=================================================="
echo

# Count test files
TOTAL_TESTS=$(grep -c "^func.*Test" test_agent_integration.go || echo "0")
echo "Total test functions defined: $TOTAL_TESTS"

# Count passed tests (if we can determine)
echo "Run time: $(date)"
echo "Test directory: $(pwd)"
echo "Configuration: Real execution enabled"
echo

echo "Integration test suite completed!"
echo "Review the output above for detailed results."
echo "For more information, see INTEGRATION_TEST_GUIDE.md"

# Cleanup temporary files
rm -f /tmp/test_*.log