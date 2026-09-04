// Package prgate implements a comprehensive PR validation gate system
// for validating pull requests against code quality, coverage, and security requirements.
//
// The gate system validates PRs against multiple criteria:
// - DCO (Developer Certificate of Origin) compliance
// - CI/CD pipeline status
// - Code coverage requirements
// - Security scan results
//
// Example usage:
//
//   config := &prgate.Config{
//       MinCoverage:    55.0,
//       RequiredChecks: []string{"ci/lint", "security"},
//       Timeout:        30 * time.Second,
//   }
//
//   gate, err := prgate.NewPRValidationGate(config)
//   if err != nil {
//       log.Fatal(err)
//   }
//
//   result, err := gate.Validate(ctx, prData)
//   if err != nil {
//       log.Fatal(err)
//   }
//
//   if result.IsValid {
//       fmt.Println("PR validation passed!")
//   } else {
//       fmt.Println("PR validation failed!")
//       for _, check := range result.CheckResults {
//           if check.Status == "fail" {
//               fmt.Printf("- %s: %s\n", check.Name, check.Message)
//           }
//       }
//   }
package prgate