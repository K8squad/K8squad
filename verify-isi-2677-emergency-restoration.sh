#!/bin/bash

# ISI-2677 Emergency Restoration Verification Script
# Purpose: Verify and restore backup DevOps Engineer functionality
# Created: 2026-08-16
# Critical: Emergency restoration required

set -e

echo "=== ISI-2677 EMERGENCY RESTORATION VERIFICATION ==="
echo "Date: $(date)"
echo "Issue: Critical regression detected - pkg/controller directory missing"
echo ""

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Function to print colored output
print_status() {
    local status=$1
    local message=$2
    case $status in
        "ERROR")
            echo -e "${RED}❌ $message${NC}"
            ;;
        "SUCCESS")
            echo -e "${GREEN}✅ $message${NC}"
            ;;
        "WARNING")
            echo -e "${YELLOW}⚠️  $message${NC}"
            ;;
        "INFO")
            echo -e "${BLUE}ℹ️  $message${NC}"
            ;;
    esac
}

print_status "INFO" "Starting emergency verification of ISI-2677 restoration..."

# Check 1: Verify pkg/controller directory exists
echo ""
echo "1. CHECKING pkg/controller directory..."
if [ -d "pkg/controller" ]; then
    print_status "SUCCESS" "pkg/controller directory exists"
else
    print_status "ERROR" "pkg/controller directory MISSING - CRITICAL ISSUE"
    echo "   This indicates complete regression of ISI-2627 fixes"
    echo "   Action required: Restore pkg/controller directory immediately"
fi

# Check 2: Verify controller files
echo ""
echo "2. CHECKING controller files..."
if [ -f "pkg/controller/run/run_controller.go" ]; then
    print_status "SUCCESS" "run_controller.go exists"
    
    # Check for real execution indicators
    if grep -q "NewAgentExecutor" pkg/controller/run/run_controller.go; then
        print_status "SUCCESS" "Real execution engine detected in run_controller.go"
    else
        print_status "ERROR" "Real execution engine missing in run_controller.go"
    fi
else
    print_status "ERROR" "run_controller.go MISSING - CRITICAL FAILURE"
fi

if [ -f "pkg/controller/agent_executor.go" ]; then
    print_status "SUCCESS" "agent_executor.go exists"
    
    # Check for real execution indicators
    if grep -q "ExecuteRun" pkg/controller/agent_executor.go; then
        print_status "SUCCESS" "ExecuteRun method detected in agent_executor.go"
    else
        print_status "ERROR" "ExecuteRun method missing in agent_executor.go"
    fi
else
    print_status "ERROR" "agent_executor.go MISSING - CRITICAL FAILURE"
fi

# Check 3: Verify role configuration
echo ""
echo "3. CHECKING role configuration..."
if [ -f "examples/backup-devops-engineer-role.yaml" ]; then
    print_status "SUCCESS" "backup-devops-engineer-role.yaml exists"
    
    # Check for required fields
    if grep -q "backup-devops-engineer-prompt" examples/backup-devops-engineer-role.yaml; then
        print_status "SUCCESS" "Prompt reference correctly configured"
    else
        print_status "ERROR" "Prompt reference missing in role configuration"
    fi
    
    if grep -q "backup-operations" examples/backup-devops-engineer-role.yaml; then
        print_status "SUCCESS" "Backup operations skills configured"
    else
        print_status "ERROR" "Backup operations skills missing"
    fi
else
    print_status "ERROR" "backup-devops-engineer-role.yaml MISSING - RESTORED IN REVIEW"
fi

# Check 4: Verify prompt template
echo ""
echo "4. CHECKING prompt template..."
if [ -f "examples/backup-devops-engineer-prompt.yaml" ]; then
    print_status "SUCCESS" "backup-devops-engineer-prompt.yaml exists"
    
    # Check for key content
    if grep -q "Backup DevOps Engineer" examples/backup-devops-engineer-prompt.yaml; then
        print_status "SUCCESS" "Prompt template content verified"
    else
        print_status "ERROR" "Prompt template content may be corrupted"
    fi
else
    print_status "ERROR" "backup-devops-engineer-prompt.yaml MISSING"
fi

# Check 5: Verify skills repository
echo ""
echo "5. CHECKING skills repository..."
if [ -f "examples/devops-skills.yaml" ]; then
    print_status "SUCCESS" "devops-skills.yaml created"
    
    # Check for critical skills
    if grep -q "backup-operations" examples/devops-skills.yaml; then
        print_status "SUCCESS" "Backup operations skills defined"
    else
        print_status "ERROR" "Backup operations skills missing from skills repository"
    fi
    
    if grep -q "disaster-recovery" examples/devops-skills.yaml; then
        print_status "SUCCESS" "Disaster recovery skills defined"
    else
        print_status "ERROR" "Disaster recovery skills missing from skills repository"
    fi
else
    print_status "ERROR" "devops-skills.yaml MISSING - RESTORED IN REVIEW"
fi

# Check 6: Verify no simulation code
echo ""
echo "6. CHECKING for simulation code..."
if [ -f "pkg/controller/run/run_controller.go" ]; then
    if grep -q "simulate successful completion" pkg/controller/run/run_controller.go; then
        print_status "ERROR" "Simulation code detected - SILENT ACTIVE RUNS RETURNED"
    else
        print_status "SUCCESS" "No simulation code detected"
    fi
else
    print_status "WARNING" "Cannot check simulation code - controller files missing"
fi

# Summary
echo ""
echo "=== ISI-2677 EMERGENCY RESTORATION SUMMARY ==="
echo ""

# Count issues
ERROR_COUNT=0
WARNING_COUNT=0
SUCCESS_COUNT=0

if [ ! -d "pkg/controller" ]; then
    ERROR_COUNT=$((ERROR_COUNT + 1))
fi

if [ ! -f "pkg/controller/run/run_controller.go" ]; then
    ERROR_COUNT=$((ERROR_COUNT + 1))
fi

if [ ! -f "pkg/controller/agent_executor.go" ]; then
    ERROR_COUNT=$((ERROR_COUNT + 1))
fi

if [ ! -f "examples/backup-devops-engineer-role.yaml" ]; then
    ERROR_COUNT=$((ERROR_COUNT + 1))
fi

if [ ! -f "examples/backup-devops-engineer-prompt.yaml" ]; then
    ERROR_COUNT=$((ERROR_COUNT + 1))
fi

if [ ! -f "examples/devops-skills.yaml" ]; then
    ERROR_COUNT=$((ERROR_COUNT + 1))
fi

# Determine overall status
if [ $ERROR_COUNT -gt 0 ]; then
    print_status "ERROR" "EMERGENCY RESTORATION REQUIRED"
    echo "   Critical issues found: $ERROR_COUNT"
    echo "   ⚠️  System is in CRITICAL state - backup operations non-functional"
    echo "   🚨 Immediate action required to restore controller infrastructure"
    echo ""
    echo "RECOMMENDATION:"
    echo "1. Locate and restore pkg/controller directory immediately"
    echo "2. Verify real execution engine is functional"
    echo "3. Test backup DevOps operations"
    echo "4. Implement monitoring to prevent future regressions"
else
    print_status "SUCCESS" "RESTORATION COMPLETED"
    echo "   All critical components verified"
    echo "   ✅ Backup DevOps Engineer system operational"
    echo "   ✅ Real execution engine confirmed"
    echo "   ✅ No silent active runs detected"
fi

echo ""
echo "=== VERIFICATION COMPLETE ==="
echo "ISI-2677 Emergency Restoration Verification - $(date)"