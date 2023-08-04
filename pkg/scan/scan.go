// Package scan implements functions and helpers to ensure the proper scan of the specified files
package scan

import (
	"context"
	"os"

	"github.com/Checkmarx/kics/assets"
	"github.com/Checkmarx/kics/pkg/engine"
	"github.com/Checkmarx/kics/pkg/engine/provider"
	"github.com/Checkmarx/kics/pkg/engine/secrets"
	"github.com/Checkmarx/kics/pkg/engine/source"
	"github.com/Checkmarx/kics/pkg/kics"
	"github.com/Checkmarx/kics/pkg/model"
	"github.com/Checkmarx/kics/pkg/parser"
	ansibleConfigParser "github.com/Checkmarx/kics/pkg/parser/ansible/ini/config"
	buildahParser "github.com/Checkmarx/kics/pkg/parser/buildah"
	dockerParser "github.com/Checkmarx/kics/pkg/parser/docker"
	protoParser "github.com/Checkmarx/kics/pkg/parser/grpc"
	jsonParser "github.com/Checkmarx/kics/pkg/parser/json"
	terraformParser "github.com/Checkmarx/kics/pkg/parser/terraform"
	yamlParser "github.com/Checkmarx/kics/pkg/parser/yaml"
	"github.com/Checkmarx/kics/pkg/resolver"
	"github.com/Checkmarx/kics/pkg/resolver/helm"
	"github.com/Checkmarx/kics/pkg/scanner"

	"github.com/rs/zerolog/log"
)

// Results represents a result generated by a single scan
type Results struct {
	Results        []model.Vulnerability
	ExtractedPaths provider.ExtractedPath
	Files          model.FileMetadatas
	FailedQueries  map[string]error
}

type executeScanParameters struct {
	services       []*kics.Service
	inspector      *engine.Inspector
	extractedPaths provider.ExtractedPath
}

func (c *Client) initScan(ctx context.Context) (*executeScanParameters, error) {
	progressBar := c.ProBarBuilder.BuildCircle("Preparing Scan Assets: ")
	go progressBar.Start()

	extractedPaths, err := c.prepareAndAnalyzePaths(ctx)
	if err != nil {
		log.Err(err)
		return nil, err
	}

	if len(extractedPaths.Path) == 0 {
		return nil, nil
	}

	querySource := source.NewFilesystemSource(
		c.ScanParams.QueriesPath,
		c.ScanParams.Platform,
		c.ScanParams.CloudProvider,
		c.ScanParams.LibrariesPath)

	queryFilter := c.createQueryFilter()

	inspector, err := engine.NewInspector(ctx,
		querySource,
		engine.DefaultVulnerabilityBuilder,
		c.Tracker,
		queryFilter,
		c.ExcludeResultsMap,
		c.ScanParams.QueryExecTimeout,
		true,
	)
	if err != nil {
		return nil, err
	}

	secretsRegexRulesContent, err := getSecretsRegexRules(c.ScanParams.SecretsRegexesPath)
	if err != nil {
		return nil, err
	}

	isCustomSecretsRegexes := len(c.ScanParams.SecretsRegexesPath) > 0

	secretsInspector, err := secrets.NewInspector(
		ctx,
		c.ExcludeResultsMap,
		c.Tracker,
		queryFilter,
		c.ScanParams.DisableSecrets,
		c.ScanParams.QueryExecTimeout,
		secretsRegexRulesContent,
		isCustomSecretsRegexes,
	)
	if err != nil {
		log.Err(err)
		return nil, err
	}

	services, err := c.createService(
		inspector,
		secretsInspector,
		extractedPaths.Path,
		c.Tracker,
		c.Storage,
		querySource,
	)
	if err != nil {
		log.Err(err)
		return nil, err
	}

	if err := progressBar.Close(); err != nil {
		log.Debug().Msgf("Failed to close progress bar: %s", err.Error())
	}

	return &executeScanParameters{
		services:       services,
		inspector:      inspector,
		extractedPaths: extractedPaths,
	}, nil
}

func (c *Client) executeScan(ctx context.Context) (*Results, error) {
	executeScanParameters, err := c.initScan(ctx)

	if err != nil {
		log.Err(err)
		return nil, err
	}

	if executeScanParameters == nil {
		return nil, nil
	}

	if err = scanner.PrepareAndScan(ctx, c.ScanParams.ScanID, *c.ProBarBuilder, executeScanParameters.services); err != nil {
		log.Err(err)
		return nil, err
	}

	failedQueries := executeScanParameters.inspector.GetFailedQueries()

	if err != nil {
		return nil, err
	}

	results, err := c.Storage.GetVulnerabilities(ctx, c.ScanParams.ScanID)
	if err != nil {
		log.Err(err)
		return nil, err
	}

	files, err := c.Storage.GetFiles(ctx, c.ScanParams.ScanID)
	if err != nil {
		log.Err(err)
		return nil, err
	}

	return &Results{
		Results:        results,
		ExtractedPaths: executeScanParameters.extractedPaths,
		Files:          files,
		FailedQueries:  failedQueries,
	}, nil
}

func getExcludeResultsMap(excludeResults []string) map[string]bool {
	excludeResultsMap := make(map[string]bool)
	for _, er := range excludeResults {
		excludeResultsMap[er] = true
	}
	return excludeResultsMap
}

func getSecretsRegexRules(regexRulesPath string) (regexRulesContent string, err error) {
	if len(regexRulesPath) > 0 {
		b, err := os.ReadFile(regexRulesPath)
		if err != nil {
			return regexRulesContent, err
		}
		regexRulesContent = string(b)
	} else {
		regexRulesContent = assets.SecretsQueryRegexRulesJSON
	}

	return regexRulesContent, nil
}

func (c *Client) createQueryFilter() *source.QueryInspectorParameters {
	excludeQueries := source.ExcludeQueries{
		ByIDs:        c.ScanParams.ExcludeQueries,
		ByCategories: c.ScanParams.ExcludeCategories,
		BySeverities: c.ScanParams.ExcludeSeverities,
	}

	includeQueries := source.IncludeQueries{
		ByIDs: c.ScanParams.IncludeQueries,
	}

	queryFilter := source.QueryInspectorParameters{
		IncludeQueries: includeQueries,
		ExcludeQueries: excludeQueries,
		InputDataPath:  c.ScanParams.InputData,
		BomQueries:     c.ScanParams.BillOfMaterials,
	}

	return &queryFilter
}

func (c *Client) createService(
	inspector *engine.Inspector,
	secretsInspector *secrets.Inspector,
	paths []string,
	t kics.Tracker,
	store kics.Storage,
	querySource *source.FilesystemSource) ([]*kics.Service, error) {
	filesSource, err := c.getFileSystemSourceProvider(paths)
	if err != nil {
		return nil, err
	}

	combinedParser, err := parser.NewBuilder().
		Add(&jsonParser.Parser{}).
		Add(&yamlParser.Parser{}).
		Add(terraformParser.NewDefaultWithVarsPath(c.ScanParams.TerraformVarsPath)).
		Add(&dockerParser.Parser{}).
		Add(&protoParser.Parser{}).
		Add(&buildahParser.Parser{}).
		Add(&ansibleConfigParser.Parser{}).
		Build(querySource.Types, querySource.CloudProviders)
	if err != nil {
		return nil, err
	}

	// combinedResolver to be used to resolve files and templates
	combinedResolver, err := resolver.NewBuilder().
		Add(&helm.Resolver{}).
		Build()
	if err != nil {
		return nil, err
	}

	services := make([]*kics.Service, 0, len(combinedParser))

	for _, parser := range combinedParser {
		services = append(
			services,
			&kics.Service{
				SourceProvider:   filesSource,
				Storage:          store,
				Parser:           parser,
				Inspector:        inspector,
				SecretsInspector: secretsInspector,
				Tracker:          t,
				Resolver:         combinedResolver,
			},
		)
	}
	return services, nil
}

func (c *Client) getFileSystemSourceProvider(paths []string) (*provider.FileSystemSourceProvider, error) {
	var excludePaths []string
	if c.ScanParams.PayloadPath != "" {
		excludePaths = append(excludePaths, c.ScanParams.PayloadPath)
	}

	if len(c.ScanParams.ExcludePaths) > 0 {
		excludePaths = append(excludePaths, c.ScanParams.ExcludePaths...)
	}

	filesSource, err := provider.NewFileSystemSourceProvider(paths, excludePaths)
	if err != nil {
		return nil, err
	}
	return filesSource, nil
}
