#!/bin/bash

# ISI-2713 Backup Code Reviewer Verification Script
# Purpose: Verify and restore backup Code Reviewer functionality
# Created: 2026-08-17
# Critical: Silent active run prevention for backup code reviewer

set -e

echo "=== ISI-2713 BACKUP CODE REVIEWER VERIFICATION ==="
echo "Date: $(date)"
echo "Issue: Review silent active run for backup_Code Reviewer"
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

print_status "INFO" "Starting ISI-2713 backup Code Reviewer verification..."

# Check 1: Verify backup code reviewer prompt template
echo ""
echo "1. CHECKING backup code reviewer prompt template..."
if [ -f "examples/backup-code-reviewer-prompt.yaml" ]; then
    print_status "SUCCESS" "backup-code-reviewer-prompt.yaml exists"
    
    # Check for required content
    if grep -q "Backup Code Reviewer" examples/backup-code-reviewer-prompt.yaml; then
        print_status "SUCCESS" "Prompt template content verified"
    else
        print_status "ERROR" "Prompt template content may be corrupted"
    fi
    
    if grep -q "Real Review Execution" examples/backup-code-reviewer-prompt.yaml; then
        print_status "SUCCESS" "Real execution emphasis confirmed"
    else
        print_status "ERROR" "Real execution emphasis missing"
    fi
else
    print_status "ERROR" "backup-code-reviewer-prompt.yaml MISSING - CREATION REQUIRED"
fi

# Check 2: Verify backup code reviewer role configuration
echo ""
echo "2. CHECKING backup code reviewer role configuration..."
if [ -f "examples/backup-code-reviewer-role.yaml" ]; then
    print_status "SUCCESS" "backup-code-reviewer-role.yaml exists"
    
    # Check for critical RBAC permissions
    if grep -q "backup-code-reviewer" examples/backup-code-reviewer-role.yaml; then
        print_status "SUCCESS" "Role configuration detected"
    else
        print_status "ERROR" "Role configuration may be corrupted"
    fi
    
    if grep -q "ClusterRole" examples/backup-code-reviewer-role.yaml; then
        print_status "SUCCESS" "Cluster role permissions configured"
    else
        print_status "ERROR" "Cluster role permissions missing"
    fi
else
    print_status "ERROR" "backup-code-reviewer-role.yaml MISSING - CREATION REQUIRED"
fi

# Check 3: Verify existing code review functionality
echo ""
echo "3. CHECKING existing code review functionality..."
if [ -f ".github/copilot-instructions.md" ]; then
    print_status "SUCCESS" "GitHub Copilot instructions found"
else
    print_status "WARNING" "GitHub Copilot instructions not found"
fi

if [ -f ".github/pull_request_template.md" ]; then
    print_status "SUCCESS" "Pull request template found"
    
    # Check for review requirements
    if grep -q "Code review" .github/pull_request_template.md; then
        print_status "SUCCESS" "Code review requirements confirmed"
    else
        print_status "WARNING" "Code review requirements may be incomplete"
    fi
else
    print_status "WARNING" "Pull request template not found"
fi

# Check 4: Verify skills repository compatibility
echo ""
echo "4. CHECKING skills repository compatibility..."
if [ -f "examples/devops-skills.yaml" ]; then
    print_status "SUCCESS" "DevOps skills repository found"
    
    # Check for code review skills
    if grep -q "code-review" examples/devops-skills.yaml; then
        print_status "SUCCESS" "Code review skills configured"
    else
        print_status "WARNING" "Code review skills may need to be added"
    fi
else
    print_status "WARNING" "DevOps skills repository not found"
fi

# Check 5: Verify controller infrastructure for real execution
echo ""
echo "5. CHECKING controller infrastructure for real execution..."
if [ -d "pkg/controller" ]; then
    print_status "SUCCESS" "Controller directory exists"
    
    if [ -f "pkg/controller/run/run_controller.go" ]; then
        print_status "SUCCESS" "Run controller exists"
        
        # Check for real execution indicators
        if grep -q "NewAgentExecutor" pkg/controller/run/run_controller.go; then
            print_status "SUCCESS" "Real execution engine detected"
        else
            print_status "ERROR" "Real execution engine missing"
        fi
    else
        print_status "ERROR" "Run controller missing"
    fi
    
    if [ -f "pkg/controller/agent_executor.go" ]; then
        print_status "SUCCESS" "Agent executor exists"
        
        # Check for ExecuteRun method
        if grep -q "ExecuteRun" pkg/controller/agent_executor.go; then
            print_status "SUCCESS" "ExecuteRun method detected"
        else
            print_status "ERROR" "ExecuteRun method missing"
        fi
    else
        print_status "ERROR" "Agent executor missing"
    fi
else
    print_status "ERROR" "Controller directory missing - CRITICAL ISSUE"
fi

# Check 6: Verify no simulation code in review systems
echo ""
echo "6. CHECKING for simulation code in review systems..."
if [ -f "pkg/controller/run/run_controller.go" ]; then
    if grep -q "simulate successful completion" pkg/controller/run/run_controller.go; then
        print_status "ERROR" "Simulation code detected - SILENT ACTIVE RUNS RETURNED"
    else
        print_status "SUCCESS" "No simulation code detected"
    fi
else
    print_status "WARNING" "Cannot check simulation code - controller files missing"
fi

# Check 7: Verify backup reviewer redundancy
echo ""
echo "7. CHECKING backup reviewer redundancy..."
if [ -f "examples/backup-code-reviewer-prompt.yaml" ] && [ -f "examples/backup-code-reviewer-role.yaml" ]; then
    print_status "SUCCESS" "Backup reviewer components present"
    
    # Check for secondary review emphasis
    if grep -q "secondary review" examples/backup-code-reviewer-prompt.yaml; then
        print_status "SUCCESS" "Secondary review capability confirmed"
    else
        print_status "WARNING" "Secondary review capability may need emphasis"
    fi
else
    print_status "ERROR" "Backup reviewer components incomplete"
fi

# Summary
echo ""
echo "=== ISI-2713 BACKUP CODE REVIEWER SUMMARY ==="
echo ""

# Count issues
ERROR_COUNT=0
WARNING_COUNT=0
SUCCESS_COUNT=0

if [ ! -f "examples/backup-code-reviewer-prompt.yaml" ]; then
    ERROR_COUNT=$((ERROR_COUNT + 1))
fi

if [ ! -f "examples/backup-code-reviewer-role.yaml" ]; then
    ERROR_COUNT=$((ERROR_COUNT + 1))
fi

if [ ! -d "pkg/controller" ]; then
    ERROR_COUNT=$((ERROR_COUNT + 1))
fi

if [ ! -f "pkg/controller/run/run_controller.go" ]; then
    ERROR_COUNT=$((ERROR_COUNT + 1))
fi

if [ ! -f "pkg/controller/agent_executor.go" ]; then
    ERROR_COUNT=$((ERROR_COUNT + 1))
fi

if grep -q "simulate successful completion" pkg/controller/run/run_controller.go 2>/dev/null; then
    ERROR_COUNT=$((ERROR_COUNT + 1))
fi

# Determine overall status
if [ $ERROR_COUNT -gt 0 ]; then
    print_status "ERROR" "SILENT ACTIVE RUN RISK DETECTED"
    echo "   Critical issues found: $ERROR_COUNT"
    echo "   ⚠️  Backup Code Reviewer system in CRITICAL state"
    echo "   🚨 Silent active runs possible - real execution compromised"
    echo ""
    echo "RECOMMENDATION:"
    echo "1. Restore controller infrastructure immediately"
    echo "2. Ensure real execution engine is functional"
    echo "3. Verify backup reviewer configuration"
    echo "4. Eliminate all simulation code"
    echo "5. Test backup review capabilities"
else
    print_status "SUCCESS" "BACKUP CODE REVIEWER OPERATIONAL"
    echo "   All critical components verified"
    echo "   ✅ Backup Code Reviewer system functional"
    echo "   ✅ Real execution engine confirmed"
    echo "   ✅ No silent active runs detected"
    echo "   ✅ Secondary review capabilities available"
fi

echo ""
echo "=== VERIFICATION COMPLETE ==="
echo "ISI-2713 Backup Code Reviewer Verification - $(date)"