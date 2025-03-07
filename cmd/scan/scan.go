package scan

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/credentials/stscreds"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	awsinternal "cloudsift/internal/aws"
	"cloudsift/internal/config"
	"cloudsift/internal/logging"
	"cloudsift/internal/output"
	"cloudsift/internal/output/html"
	"cloudsift/internal/worker"
)

type scanOptions struct {
	regions             string
	scanners            string
	output              string // filesystem or s3
	outputFormat        string // html or json
	bucket              string
	bucketRegion        string
	organizationRole    string // Role to assume for listing organization accounts
	scannerRole         string // Role to assume for scanning accounts
	daysUnused          int    // Number of days a resource must be unused to be reported
	ignoreResourceIDs   string
	ignoreResourceNames string
	ignoreTags          string
	accounts            string // Comma-separated list of account IDs to scan
}

type scannerProgress struct {
	AccountID   string
	AccountName string
	Region      string
	Scanner     string
	ResultCount int // Number of scan results found
}

type scannerProgressMap struct {
	sync.RWMutex
	progress map[string]*scannerProgress // key is accountID:region:scanner
}

func newScannerProgressMap() *scannerProgressMap {
	return &scannerProgressMap{
		progress: make(map[string]*scannerProgress),
	}
}

func (s *scannerProgressMap) startScanner(accountID, accountName, region, scanner string) {
	s.Lock()
	defer s.Unlock()
	key := fmt.Sprintf("%s:%s:%s", accountID, region, scanner)
	s.progress[key] = &scannerProgress{
		AccountID:   accountID,
		AccountName: accountName,
		Region:      region,
		Scanner:     scanner,
		ResultCount: 0,
	}
}

func (s *scannerProgressMap) updateResultCount(accountID, region, scanner string, count int) {
	s.Lock()
	defer s.Unlock()
	key := fmt.Sprintf("%s:%s:%s", accountID, region, scanner)
	if prog, exists := s.progress[key]; exists {
		prog.ResultCount = count
	}
}

func (s *scannerProgressMap) completeScanner(accountID, region, scanner string) {
	s.Lock()
	defer s.Unlock()
	key := fmt.Sprintf("%s:%s:%s", accountID, region, scanner)
	delete(s.progress, key)
}

func (s *scannerProgressMap) getRunning() []*scannerProgress {
	s.RLock()
	defer s.RUnlock()
	var running []*scannerProgress
	for _, prog := range s.progress {
		running = append(running, prog)
	}
	return running
}

// NewScanCmd creates the scan command
func NewScanCmd() *cobra.Command {
	opts := &scanOptions{}

	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Scan AWS resources",
		Long: `Scan AWS resources for potential cost savings.

When no scanners or regions are specified, all available scanners will be run in all available regions.
When no organization-role is specified, only the current account will be scanned.
When both organization-role and scanner-role are specified, all accounts in the organization will be scanned.
When accounts is specified, only the specified accounts will be scanned. The accounts must exist in the organization.

Examples:
  # Scan all resources in all regions of current account
  cloudsift scan

  # Scan EBS volumes in us-west-2 of current account using a specific profile
  cloudsift scan --scanners ebs-volumes --regions us-west-2 --profile my-profile

  # Scan multiple resource types in multiple regions of all organization accounts
  cloudsift scan --scanners ebs-volumes,ebs-snapshots --regions us-west-2,us-east-1 --organization-role OrganizationAccessRole --scanner-role SecurityAuditRole

  # Scan specific accounts in the organization
  cloudsift scan --accounts 123456789012,098765432109 --organization-role OrganizationAccessRole --scanner-role SecurityAuditRole

  # Output HTML report to S3
  cloudsift scan --output s3 --output-format html --bucket my-bucket --bucket-region us-west-2

  # Output JSON results to S3
  cloudsift scan --output s3 --output-format json --bucket my-bucket --bucket-region us-west-2`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Command line flags should take precedence over config and env vars
			if cmd.Flags().Changed("regions") {
				config.Config.ScanRegions = opts.regions
			}
			if cmd.Flags().Changed("scanners") {
				config.Config.ScanScanners = opts.scanners
			}
			if cmd.Flags().Changed("output") {
				config.Config.ScanOutput = opts.output
			}
			if cmd.Flags().Changed("output-format") {
				config.Config.ScanOutputFormat = opts.outputFormat
			}
			if cmd.Flags().Changed("bucket") {
				config.Config.ScanBucket = opts.bucket
			}
			if cmd.Flags().Changed("bucket-region") {
				config.Config.ScanBucketRegion = opts.bucketRegion
			}
			if cmd.Flags().Changed("organization-role") {
				config.Config.OrganizationRole = opts.organizationRole
			}
			if cmd.Flags().Changed("scanner-role") {
				config.Config.ScannerRole = opts.scannerRole
			}
			if cmd.Flags().Changed("days-unused") {
				config.Config.ScanDaysUnused = opts.daysUnused
			}
			if cmd.Flags().Changed("ignore-resource-ids") {
				config.Config.ScanIgnoreResourceIDs = strings.Split(opts.ignoreResourceIDs, ",")
			}
			if cmd.Flags().Changed("ignore-resource-names") {
				config.Config.ScanIgnoreResourceNames = strings.Split(opts.ignoreResourceNames, ",")
			}
			if cmd.Flags().Changed("ignore-tags") {
				tags := make(map[string]string)
				for _, tag := range strings.Split(opts.ignoreTags, ",") {
					parts := strings.SplitN(tag, "=", 2)
					if len(parts) == 2 {
						tags[parts[0]] = parts[1]
					}
				}
				config.Config.ScanIgnoreTags = tags
			}
			if cmd.Flags().Changed("accounts") {
				config.Config.ScanAccounts = strings.Split(opts.accounts, ",")
			}

			// Bind scan-specific flags to viper
			if err := viper.BindPFlag("scan.regions", cmd.Flags().Lookup("regions")); err != nil {
				return err
			}
			if err := viper.BindPFlag("scan.scanners", cmd.Flags().Lookup("scanners")); err != nil {
				return err
			}
			if err := viper.BindPFlag("scan.output", cmd.Flags().Lookup("output")); err != nil {
				return err
			}
			if err := viper.BindPFlag("scan.output_format", cmd.Flags().Lookup("output-format")); err != nil {
				return err
			}
			if err := viper.BindPFlag("scan.bucket", cmd.Flags().Lookup("bucket")); err != nil {
				return err
			}
			if err := viper.BindPFlag("scan.bucket_region", cmd.Flags().Lookup("bucket-region")); err != nil {
				return err
			}
			if err := viper.BindPFlag("scan.days_unused", cmd.Flags().Lookup("days-unused")); err != nil {
				return err
			}
			if err := viper.BindPFlag("scan.ignore.resource_ids", cmd.Flags().Lookup("ignore-resource-ids")); err != nil {
				return err
			}
			if err := viper.BindPFlag("scan.ignore.resource_names", cmd.Flags().Lookup("ignore-resource-names")); err != nil {
				return err
			}
			if err := viper.BindPFlag("scan.ignore.tags", cmd.Flags().Lookup("ignore-tags")); err != nil {
				return err
			}
			if err := viper.BindPFlag("scan.accounts", cmd.Flags().Lookup("accounts")); err != nil {
				return err
			}

			// Log configuration sources after binding all flags
			config.LogConfigurationSources(true, cmd)

			// Validate output format
			switch opts.outputFormat {
			case "json", "html":
				// Valid formats
			default:
				return fmt.Errorf("invalid output format: %s", opts.outputFormat)
			}

			// Validate output type
			switch opts.output {
			case "filesystem", "s3":
				// Valid output types
			default:
				return fmt.Errorf("invalid output type: %s", opts.output)
			}

			// Validate S3 parameters
			if opts.output == "s3" {
				if opts.bucket == "" {
					return fmt.Errorf("--bucket is required when --output=s3")
				}
				if opts.bucketRegion == "" {
					return fmt.Errorf("--bucket-region is required when --output=s3")
				}
			}

			return runScan(cmd, opts)
		},
	}

	cmd.Flags().StringVar(&opts.regions, "regions", "", "Comma-separated list of regions to scan (default: all available regions)")
	cmd.Flags().StringVar(&opts.scanners, "scanners", "", "Comma-separated list of scanners to run (default: all available scanners)")
	cmd.Flags().StringVar(&opts.output, "output", "filesystem", "Output type (filesystem, s3)")
	cmd.Flags().StringVarP(&opts.outputFormat, "output-format", "o", "html", "Output format (json, html)")
	cmd.Flags().StringVar(&opts.bucket, "bucket", "", "S3 bucket name (required when --output=s3)")
	cmd.Flags().StringVar(&opts.bucketRegion, "bucket-region", "", "S3 bucket region (required when --output=s3)")
	cmd.Flags().StringVar(&opts.organizationRole, "organization-role", "", "Role to assume for listing organization accounts")
	cmd.Flags().StringVar(&opts.scannerRole, "scanner-role", "", "Role to assume for scanning accounts")
	cmd.Flags().IntVar(&opts.daysUnused, "days-unused", 90, "Number of days a resource must be unused to be reported")
	cmd.Flags().StringVar(&opts.ignoreResourceIDs, "ignore-resource-ids", "", "Comma-separated list of resource IDs to ignore (case-insensitive)")
	cmd.Flags().StringVar(&opts.ignoreResourceNames, "ignore-resource-names", "", "Comma-separated list of resource names to ignore (case-insensitive)")
	cmd.Flags().StringVar(&opts.ignoreTags, "ignore-tags", "", "Comma-separated list of tags to ignore in KEY=VALUE format (case-insensitive)")
	cmd.Flags().StringVar(&opts.accounts, "accounts", "", "Comma-separated list of account IDs to scan (default: all accounts in organization)")

	return cmd
}

type scanResult struct {
	AccountID   string                             `json:"account_id"`
	AccountName string                             `json:"account_name"`
	Results     map[string]awsinternal.ScanResults `json:"results"` // Map of scanner name to results
}

// isIAMScanner returns true if the scanner is for IAM resources
func isIAMScanner(scanner awsinternal.Scanner) bool {
	return scanner.Label() == "IAM Roles" || scanner.Label() == "IAM Users"
}

func getScanners(scannerList string) ([]awsinternal.Scanner, []string, error) {
	var scanners []awsinternal.Scanner
	var invalidScanners []string

	// If no scanners specified, get all available scanners
	if scannerList == "" {
		// If no scanners specified, get all available scanners
		names := awsinternal.DefaultRegistry.ListScanners()
		if len(names) == 0 {
			return nil, nil, fmt.Errorf("no scanners available in registry")
		}

		for _, name := range names {
			scanner, err := awsinternal.DefaultRegistry.GetScanner(name)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get scanner '%s': %w", name, err)
			}
			scanners = append(scanners, scanner)
		}
		return scanners, invalidScanners, nil
	}

	// Parse comma-separated list of scanners
	names := strings.Split(scannerList, ",")
	for _, name := range names {
		scanner, err := awsinternal.DefaultRegistry.GetScanner(name)
		if err != nil {
			// Track invalid scanner but continue processing
			invalidScanners = append(invalidScanners, name)
			continue
		}
		scanners = append(scanners, scanner)
	}

	return scanners, invalidScanners, nil
}

func runScan(cmd *cobra.Command, opts *scanOptions) error {
	// Validate S3 access first if using S3 output
	if opts.output == "s3" {
		if opts.bucket == "" {
			return fmt.Errorf("S3 bucket not specified. Use --bucket flag to specify the S3 bucket")
		}
		if err := validateS3Access(opts.bucket, opts.bucketRegion, opts.organizationRole); err != nil {
			return fmt.Errorf("S3 bucket validation failed: %w", err)
		}
	}

	// Get and validate scanners
	scanners, invalidScanners, err := getScanners(opts.scanners)
	if err != nil {
		logging.Error("Failed to get scanners", err, map[string]interface{}{
			"scanners": opts.scanners,
		})
		scanners = []awsinternal.Scanner{} // Continue with empty scanner list
	}

	if len(invalidScanners) > 0 {
		logging.Warn("Invalid scanners specified", map[string]interface{}{
			"invalid_scanners": invalidScanners,
		})
	}

	if len(scanners) == 0 {
		if len(invalidScanners) > 0 {
			// Exit immediately if no valid scanners and at least one invalid scanner
			return fmt.Errorf("no valid scanners found and invalid scanners specified: %s", strings.Join(invalidScanners, ", "))
		}
		logging.Warn("No scanners available, scan will be skipped", nil)
	}

	// Create base session and get accounts
	var baseSession *session.Session
	var accounts []awsinternal.Account

	// Create a session with organization role for cost estimator
	var costEstimatorSession *session.Session
	var costErr error
	if opts.organizationRole != "" {
		costEstimatorSession, costErr = awsinternal.GetSessionChain(opts.organizationRole, "", "", "us-east-1")
		if costErr != nil {
			logging.Error("Failed to create cost estimator session with org role", costErr, map[string]interface{}{
				"organization_role": opts.organizationRole,
			})
			// Fall back to root profile
			logging.Info("Falling back to root profile for cost estimator")
			costEstimatorSession, costErr = awsinternal.NewSession(config.Config.Profile, "us-east-1")
			if costErr != nil {
				logging.Error("Failed to create cost estimator session", costErr, nil)
				return nil // Return nil to continue without failing
			}
		}
	} else {
		costEstimatorSession, costErr = awsinternal.NewSession(config.Config.Profile, "us-east-1")
		if costErr != nil {
			logging.Error("Failed to create cost estimator session", costErr, nil)
			return nil // Return nil to continue without failing
		}
	}

	// Initialize cost estimator with the session
	if err := awsinternal.InitializeDefaultCostEstimator(costEstimatorSession); err != nil {
		logging.Error("Failed to initialize cost estimator", err, nil)
		return nil // Return nil to continue without failing
	}

	if opts.organizationRole != "" && opts.scannerRole != "" {
		logging.Info("Creating organization session", map[string]interface{}{
			"organization_role": opts.organizationRole,
			"scanner_role":      opts.scannerRole,
		})
		// Create org role session for listing accounts
		baseSession, err = awsinternal.GetSessionChain(opts.organizationRole, "", "", "us-west-2")
		if err != nil {
			logging.Error("Failed to create organization session", err, map[string]interface{}{
				"organization_role": opts.organizationRole,
			})
			// Fall back to current session
			logging.Info("Falling back to current session")
			baseSession, err = awsinternal.NewSession(config.Config.Profile, "")
			if err != nil {
				logging.Error("Failed to create base session", err, nil)
				return nil // Return nil to continue without failing
			}
		}
	} else {
		logging.Debug("Using current session", nil)
		// Use current session with profile
		baseSession, err = awsinternal.NewSession(config.Config.Profile, "")
		if err != nil {
			logging.Error("Failed to create base session", err, nil)
			return nil // Return nil to continue without failing
		}
	}

	// Get accounts
	if opts.organizationRole != "" && opts.scannerRole != "" {
		accounts, err = awsinternal.ListAccountsWithSession(baseSession)
		if err != nil {
			logging.Error("Failed to list organization accounts", err, map[string]interface{}{
				"organization_role": opts.organizationRole,
			})
			// Fall back to current account
			logging.Info("Falling back to current account")
			accounts, err = awsinternal.ListCurrentAccount(baseSession)
			if err != nil {
				logging.Error("Failed to get current account", err, nil)
				return nil // Return nil to continue without failing
			}
		}
	} else {
		// Get current account only
		accounts, err = awsinternal.ListCurrentAccount(baseSession)
		if err != nil {
			logging.Error("Failed to get current account", err, nil)
			return nil // Return nil to continue without failing
		}
	}

	// Filter accounts by specified account IDs
	if opts.accounts != "" {
		requestedAccounts := strings.Split(opts.accounts, ",")
		accountMap := make(map[string]bool)
		for _, account := range accounts {
			accountMap[account.ID] = true
		}

		// Validate all requested accounts exist
		var invalidAccounts []string
		for _, accountID := range requestedAccounts {
			accountID = strings.TrimSpace(accountID)
			if !accountMap[accountID] {
				invalidAccounts = append(invalidAccounts, accountID)
			}
		}
		if len(invalidAccounts) > 0 {
			logging.Warn("Some requested accounts do not exist in the organization", map[string]interface{}{
				"invalid_accounts": invalidAccounts,
			})
		}

		// Filter to only requested accounts
		var filteredAccounts []awsinternal.Account
		requestedAccountMap := make(map[string]bool)
		for _, accountID := range requestedAccounts {
			requestedAccountMap[strings.TrimSpace(accountID)] = true
		}
		for _, account := range accounts {
			if requestedAccountMap[account.ID] {
				filteredAccounts = append(filteredAccounts, account)
			}
		}
		accounts = filteredAccounts

		if len(accounts) == 0 {
			return fmt.Errorf("none of the specified accounts exist in the organization")
		}
	}

	// Create sessions for each account
	accountSessions := make(map[string]*session.Session)
	var authenticatedAccounts []awsinternal.Account // Track accounts that successfully authenticated
	for _, account := range accounts {
		if opts.organizationRole != "" && opts.scannerRole != "" {
			// Assume scanner role in target account using org session
			scannerRoleARN := fmt.Sprintf("arn:aws:iam::%s:role/%s", account.ID, opts.scannerRole)
			scannerCreds := stscreds.NewCredentials(baseSession, scannerRoleARN)
			scanSession, err := session.NewSession(aws.NewConfig().WithCredentials(scannerCreds))
			if err != nil {
				logging.Warn("Failed to assume scanner role", map[string]interface{}{
					"error":        err.Error(),
					"account_id":   account.ID,
					"account_name": account.Name,
					"role_arn":     scannerRoleARN,
				})
				continue // Skip this account
			}

			// Verify scanner role assumption
			stsSvc := sts.New(scanSession)
			identity, err := stsSvc.GetCallerIdentity(&sts.GetCallerIdentityInput{})
			if err != nil {
				logging.Warn("Failed to verify scanner role assumption", map[string]interface{}{
					"error":        err.Error(),
					"account_id":   account.ID,
					"account_name": account.Name,
					"role_arn":     scannerRoleARN,
				})
				continue // Skip this account
			}
			logging.Info("Successfully assumed scanner role", map[string]interface{}{
				"account_id":   account.ID,
				"account_name": account.Name,
				"role_arn":     *identity.Arn,
			})

			accountSessions[account.ID] = scanSession
			authenticatedAccounts = append(authenticatedAccounts, account)
		} else {
			// Use base session for current account
			accountSessions[account.ID] = baseSession
			authenticatedAccounts = append(authenticatedAccounts, account)
		}
	}

	if len(accountSessions) == 0 {
		logging.Warn("No valid sessions created for any accounts, scan will be skipped", nil)
		return nil
	}

	// Use only authenticated accounts from here on
	accounts = authenticatedAccounts

	// Get and validate regions
	var regions []string
	if opts.regions == "" {
		// If no regions specified, get all available regions
		regions, err = awsinternal.GetAvailableRegions(accountSessions[accounts[0].ID])
		if err != nil {
			logging.Error("Failed to get available regions", err, nil)
			return nil // Return nil to continue without failing
		}
	} else {
		// Parse and validate comma-separated list of regions
		regions = strings.Split(opts.regions, ",")
		if err := awsinternal.ValidateRegions(accountSessions[accounts[0].ID], regions); err != nil {
			logging.Error("Invalid regions", err, map[string]interface{}{
				"regions": opts.regions,
			})
			return nil // Return nil to continue without failing
		}
	}

	// Initialize results map
	accountResults := make(map[string]*scanResult)
	for _, account := range accounts {
		accountResults[account.ID] = &scanResult{
			AccountID:   account.ID,
			AccountName: account.Name,
			Results:     make(map[string]awsinternal.ScanResults),
		}
	}

	// Create tasks for each scanner+region+account combination
	var tasks []worker.Task
	var resultsMutex sync.Mutex
	progressMap := newScannerProgressMap()
	actualTasks := 0

	// Initialize shared worker pool
	if err := worker.InitSharedPool(config.Config.MaxWorkers); err != nil {
		return fmt.Errorf("failed to initialize worker pool: %w", err)
	}
	workerPool := worker.GetSharedPool()

	// Log scan start with configuration
	var scannerNames []string
	for _, s := range scanners {
		scannerNames = append(scannerNames, s.Label())
	}

	// Convert accounts to the format expected by the logger
	var accountInfo []logging.Account
	for _, acc := range accounts {
		accountInfo = append(accountInfo, logging.Account{
			ID:   acc.ID,
			Name: acc.Name,
		})
	}

	startTime := time.Now()
	logging.ScanStart(scannerNames, accountInfo, regions)

	// Start progress logger
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		tickDuration := 30 * time.Second
		ticker := time.NewTicker(tickDuration)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				running := progressMap.getRunning()
				if len(running) > 0 {
					// Only emit progress if no logs in the last tick interval
					lastLog := logging.GetLastLogTime()
					if time.Since(lastLog) >= tickDuration {
						// Get worker pool metrics
						metrics := workerPool.GetMetrics()
						activeWorkers := metrics.CurrentWorkers
						maxWorkers := int64(config.Config.MaxWorkers)
						freeWorkers := maxWorkers - activeWorkers
						utilization := float64(activeWorkers) / float64(maxWorkers) * 100

						// Log header with detailed worker stats
						logging.Progress(fmt.Sprintf("Pending Scanners (Workers: %d active (%d%% utilized), %d idle of %d total):",
							activeWorkers, int(utilization), freeWorkers, maxWorkers), nil)

						// Sort scanners by account ID and scanner name for consistent output
						sort.Slice(running, func(i, j int) bool {
							if running[i].AccountID != running[j].AccountID {
								return running[i].AccountID < running[j].AccountID
							}
							return running[i].Scanner < running[j].Scanner
						})

						// Log each scanner on its own line
						for _, prog := range running {
							region := prog.Region
							if region == "us-east-1" && (prog.Scanner == "IAM Roles" || prog.Scanner == "IAM Users") {
								region = "global"
							}

							logging.Progress(fmt.Sprintf("  %s: %s (%s) in %s - %d results found",
								prog.Scanner,
								prog.AccountName,
								prog.AccountID,
								region,
								prog.ResultCount,
							), nil)
						}

						// Log completion stats if any tasks have completed
						if metrics.CompletedTasks > 0 {
							avgExecMs := metrics.AverageExecutionMs
							tasksPerSec := float64(metrics.CompletedTasks) / float64(metrics.AverageExecutionMs) * 1000
							logging.Progress(fmt.Sprintf("  Stats: %d completed, %d failed, %.1f tasks/sec, avg %.1fs per task",
								metrics.CompletedTasks,
								metrics.FailedTasks,
								tasksPerSec,
								float64(avgExecMs)/1000.0,
							), nil)
						}
					}
				}
			}
		}
	}()

	for _, scanner := range scanners {
		// For IAM scanners, we only need to scan us-east-1 since IAM is global
		scanRegions := regions
		if isIAMScanner(scanner) {
			scanRegions = []string{"us-east-1"}
		}

		for _, region := range scanRegions {
			for _, account := range accounts {
				actualTasks++
				scanner := scanner // Create new variable for closure
				region := region
				account := account

				tasks = append(tasks, worker.Task(func(ctx context.Context) error {
					// For IAM scanners, always log region as "global"
					logRegion := region
					if isIAMScanner(scanner) {
						logRegion = "global"
					}
					logging.ScannerStart(scanner.Label(), account.ID, account.Name, logRegion)

					// Start tracking scanner progress
					progressMap.startScanner(account.ID, account.Name, logRegion, scanner.Label())
					defer progressMap.completeScanner(account.ID, logRegion, scanner.Label())

					// Get the account's base session and create regional session
					scanSession := accountSessions[account.ID]
					regionSession, err := awsinternal.GetSessionInRegion(scanSession, region)
					if err != nil {
						logging.ScannerError(scanner.Label(), account.ID, account.Name, logRegion, err)
						return fmt.Errorf("failed to create regional session for account %s: %w", account.ID, err)
					}
					logging.Debug("Created regional session", map[string]interface{}{
						"region": region,
					})

					results, err := scanner.Scan(awsinternal.ScanOptions{
						Region:     region,
						DaysUnused: opts.daysUnused,
						Session:    regionSession,
					})
					if err != nil {
						logging.ScannerError(scanner.Label(), account.ID, account.Name, logRegion, err)
						return err
					}

					// Filter results based on ignore list
					var filteredResults awsinternal.ScanResults
					for _, result := range results {
						// Check if resource ID is in ignore list
						shouldIgnore := false
						for _, ignoreID := range config.Config.ScanIgnoreResourceIDs {
							if strings.EqualFold(result.ResourceID, ignoreID) {
								logging.Debug("Ignoring resource by ID", map[string]interface{}{
									"resource_id": result.ResourceID,
									"scanner":     scanner.Label(),
									"account_id":  account.ID,
									"region":      logRegion,
								})
								shouldIgnore = true
								break
							}
						}

						// Check if resource name is in ignore list
						if !shouldIgnore {
							for _, ignoreName := range config.Config.ScanIgnoreResourceNames {
								if strings.EqualFold(result.ResourceName, ignoreName) {
									logging.Debug("Ignoring resource by name", map[string]interface{}{
										"resource_name": result.ResourceName,
										"scanner":       scanner.Label(),
										"account_id":    account.ID,
										"region":        logRegion,
									})
									shouldIgnore = true
									break
								}
							}
						}

						// Check if any resource tags match ignore list
						if !shouldIgnore && len(result.Tags) > 0 {
							for ignoreKey, ignoreValue := range config.Config.ScanIgnoreTags {
								// Convert tag key and value to lowercase for case-insensitive comparison
								for tagKey, tagValue := range result.Tags {
									if strings.EqualFold(tagKey, ignoreKey) && strings.EqualFold(tagValue, ignoreValue) {
										logging.Debug("Ignoring resource by tag", map[string]interface{}{
											"resource_id": result.ResourceID,
											"tag_key":     ignoreKey,
											"tag_value":   ignoreValue,
											"scanner":     scanner.Label(),
											"account_id":  account.ID,
											"region":      logRegion,
										})
										shouldIgnore = true
										break
									}
								}
								if shouldIgnore {
									break
								}
							}
						}

						if !shouldIgnore {
							filteredResults = append(filteredResults, result)
						}
					}

					// Update result count with filtered results
					progressMap.updateResultCount(account.ID, logRegion, scanner.Label(), len(filteredResults))

					// Add account and region info to each result
					for i := range filteredResults {
						if filteredResults[i].Details == nil {
							filteredResults[i].Details = make(map[string]interface{})
						}
						filteredResults[i].AccountID = account.ID
						filteredResults[i].AccountName = account.Name
						// For IAM scanners, set region as "global", otherwise use actual region
						if isIAMScanner(scanner) {
							filteredResults[i].Details["region"] = "global"
						} else {
							filteredResults[i].Details["region"] = region
						}
					}

					// Safely append results
					resultsMutex.Lock()
					if accountResults[account.ID].Results[scanner.Label()] == nil {
						accountResults[account.ID].Results[scanner.Label()] = filteredResults
					} else {
						accountResults[account.ID].Results[scanner.Label()] = append(accountResults[account.ID].Results[scanner.Label()], filteredResults...)
					}
					resultsMutex.Unlock()

					// Log completion with results
					resultInterfaces := make([]interface{}, len(filteredResults))
					for i, r := range filteredResults {
						resultInterfaces[i] = r
					}
					logging.ScannerComplete(scanner.Label(), account.ID, account.Name, logRegion, resultInterfaces)

					return nil
				}))
			}
		}
	}

	// Execute tasks using the worker pool
	workerPool.ExecuteTasks(tasks)

	// Verify task count matches expected scans
	metrics := workerPool.GetMetrics()

	// Get worker pool metrics
	logging.Info("Worker pool metrics", map[string]interface{}{
		"total_tasks":        metrics.TotalTasks,
		"completed_tasks":    metrics.CompletedTasks,
		"failed_tasks":       metrics.FailedTasks,
		"peak_workers":       metrics.PeakWorkers,
		"avg_execution_ms":   metrics.AverageExecutionMs,
		"tasks_per_second":   float64(metrics.CompletedTasks) / float64(metrics.AverageExecutionMs) * 1000,
		"worker_utilization": float64(metrics.PeakWorkers) / float64(config.Config.MaxWorkers) * 100,
	})

	// Output results
	switch opts.output {
	case "filesystem":
		switch opts.outputFormat {
		case "json":
			// Use writer for JSON filesystem output
			writer := output.NewWriter(output.Config{
				Type:      output.FileSystem,
				OutputDir: "output",
			})

			for accountID, result := range accountResults {
				if err := writer.Write(accountID, result); err != nil {
					logging.Error("Error writing results for account", err, map[string]interface{}{
						"account_id": accountID,
					})
				}
			}
		case "html":
			// Create reports directory if it doesn't exist
			if err := os.MkdirAll("reports", 0755); err != nil {
				logging.Error("Error creating reports directory", err, nil)
			}

			// Collect all results
			var allResults []awsinternal.ScanResult
			for _, accountResult := range accountResults {
				for _, scannerResults := range accountResult.Results {
					allResults = append(allResults, scannerResults...)
				}
			}

			// Calculate scan metrics
			duration := time.Since(startTime).Seconds()
			metrics := html.ScanMetrics{
				CompletedScans:     metrics.CompletedTasks,
				FailedScans:        metrics.FailedTasks,
				TotalRunTime:       duration,
				AvgScansPerSecond:  float64(metrics.CompletedTasks) / duration,
				CompletedAt:        time.Now(),
				PeakWorkers:        metrics.PeakWorkers,
				MaxWorkers:         config.Config.MaxWorkers,
				WorkerUtilization:  float64(metrics.PeakWorkers) / float64(config.Config.MaxWorkers) * 100,
				AvgExecutionTimeMs: metrics.AverageExecutionMs,
				TasksPerSecond:     float64(metrics.CompletedTasks) / float64(metrics.AverageExecutionMs) * 1000,
			}

			outputPath := "reports/scan_report.html"
			if err := html.WriteHTML(allResults, outputPath, metrics); err != nil {
				logging.Error("Error writing HTML output", err, map[string]interface{}{
					"output_path": outputPath,
				})
			}
			fmt.Printf("HTML report written to %s\n", outputPath)
		}
	case "s3":
		writer := output.NewWriter(output.Config{
			Type:             output.S3,
			S3Bucket:         opts.bucket,
			S3Region:         opts.bucketRegion,
			OrganizationRole: opts.organizationRole,
		})

		// Write results for each account
		for accountID, result := range accountResults {
			outputData := scanResult{
				AccountID:   accountID,
				AccountName: accounts[0].Name,
				Results:     result.Results,
			}

			data, err := json.Marshal(outputData)
			if err != nil {
				logging.Error("Error marshaling scan results", err, map[string]interface{}{
					"account_id": accountID,
				})
				continue
			}

			if err := writer.Write(accountID, data); err != nil {
				logging.Error("Error writing scan results to S3", err, map[string]interface{}{
					"account_id": accountID,
					"bucket":     opts.bucket,
				})
				continue
			}

			logging.Info("Successfully wrote scan results to S3", map[string]interface{}{
				"account_id": accountID,
				"bucket":     opts.bucket,
			})
		}
	}

	logging.ScanComplete(len(accountResults))
	return nil
}

// getRoleARN returns the full ARN for a role. If the input is already an ARN, returns it as is.
func getRoleARN(sess *session.Session, roleName string) (string, error) {
	// If it's already an ARN, return it
	if strings.HasPrefix(roleName, "arn:aws:iam::") {
		return roleName, nil
	}

	// Get the account ID using STS
	stsClient := sts.New(sess)
	result, err := stsClient.GetCallerIdentity(&sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fmt.Errorf("failed to get account ID: %w", err)
	}

	// Construct the role ARN
	return fmt.Sprintf("arn:aws:iam::%s:role/%s", *result.Account, roleName), nil
}

// getSessionWithOrgRole creates an AWS session and assumes the organization role if specified
func getSessionWithOrgRole(region, orgRole string) (*session.Session, error) {
	// Create base session
	sess, err := awsinternal.GetSession("", region)
	if err != nil {
		return nil, fmt.Errorf("failed to create AWS session: %w", err)
	}

	// If organization role is specified, assume it
	if orgRole != "" {
		// Get full role ARN
		roleARN, err := getRoleARN(sess, orgRole)
		if err != nil {
			return nil, fmt.Errorf("failed to get role ARN: %w", err)
		}

		// Create assume role input
		roleSessionName := fmt.Sprintf("cloudsift-scan-%d", time.Now().Unix())
		input := &sts.AssumeRoleInput{
			RoleArn:         aws.String(roleARN),
			RoleSessionName: aws.String(roleSessionName),
		}

		// Assume the role
		result, err := sts.New(sess).AssumeRole(input)
		if err != nil {
			return nil, fmt.Errorf("failed to assume role: %w", err)
		}

		// Create new session with temporary credentials
		sess, err = session.NewSession(&aws.Config{
			Region: aws.String(region),
			Credentials: credentials.NewStaticCredentials(
				*result.Credentials.AccessKeyId,
				*result.Credentials.SecretAccessKey,
				*result.Credentials.SessionToken,
			),
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create session with assumed role: %w", err)
		}
		logging.Debug("Assumed organization role", map[string]interface{}{
			"role":   roleARN,
			"region": region,
		})
	}

	return sess, nil
}

// validateS3Access validates that we can write to the specified S3 bucket
func validateS3Access(bucket, region string, orgRole string) error {
	logging.Info("Starting S3 bucket access validation", map[string]interface{}{
		"bucket": bucket,
		"region": region,
	})

	// Create AWS session with organization role if specified
	sess, err := getSessionWithOrgRole(region, orgRole)
	if err != nil {
		logging.Error("Failed to create AWS session", err, map[string]interface{}{
			"bucket": bucket,
			"region": region,
		})
		return fmt.Errorf("failed to create AWS session: %w", err)
	}
	// Create S3 client
	s3Client := s3.New(sess)

	// Use a specific validation path that won't conflict with scan results
	testKey := ".cloudsift_validation"

	// Try to upload a test file with required encryption
	_, err = s3Client.PutObject(&s3.PutObjectInput{
		Bucket:               aws.String(bucket),
		Key:                  aws.String(testKey),
		Body:                 bytes.NewReader([]byte("test")),
		ServerSideEncryption: aws.String("aws:kms"),
	})
	if err != nil {
		logging.Error("Failed to write test file to S3", err, map[string]interface{}{
			"bucket": bucket,
			"key":    testKey,
		})
		return fmt.Errorf("failed to validate S3 bucket access: %w", err)
	}
	logging.Info("Successfully wrote test file to S3", map[string]interface{}{
		"bucket": bucket,
		"key":    testKey,
	})

	// Clean up the test file
	logging.Debug("Attempting to clean up test file", map[string]interface{}{
		"bucket": bucket,
		"key":    testKey,
	})
	_, err = s3Client.DeleteObject(&s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(testKey),
	})
	if err != nil {
		logging.Warn("Failed to clean up S3 test file", err, map[string]interface{}{
			"bucket": bucket,
			"key":    testKey,
		})
	} else {
		logging.Info("Successfully cleaned up test file", map[string]interface{}{
			"bucket": bucket,
			"key":    testKey,
		})
	}

	logging.Info("S3 bucket access validation complete", map[string]interface{}{
		"bucket": bucket,
		"region": region,
	})
	return nil
}
