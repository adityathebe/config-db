package aws

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/flanksource/config-db/api/v1"
	"github.com/flanksource/config-db/db"
	"github.com/flanksource/config-db/db/models"
	athena "github.com/uber/athenadriver/go"
)

const costQueryTemplate = `
    WITH
        max_end_date AS (
            SELECT MAX(line_item_usage_end_date) as end_date FROM $table
            WHERE line_item_resource_id = @id AND line_item_product_code = @product_code
        ),
        results AS (
            SELECT line_item_unblended_cost, line_item_usage_start_date FROM $table
            WHERE line_item_resource_id = @id AND line_item_product_code = @product_code
        )
    SELECT
        SUM(CASE WHEN line_item_usage_start_date >= (SELECT date_add('hour', -1, end_date) FROM max_end_date) THEN line_item_unblended_cost
            ELSE 0 END) as hourly,
        SUM(CASE WHEN line_item_usage_start_date >= (SELECT date_add('day', -1, end_date) FROM max_end_date) THEN line_item_unblended_cost
            ELSE 0 END) as daily,
        SUM(CASE WHEN line_item_usage_start_date >= (SELECT date_add('day', -7, end_date) FROM max_end_date) THEN line_item_unblended_cost
            ELSE 0 END) as weekly,
        SUM(CASE WHEN line_item_usage_start_date >= (SELECT date_add('day', -30, end_date) FROM max_end_date) THEN line_item_unblended_cost
            ELSE 0 END) as monthly
    FROM results
`

func getJSONKey(body, key string) (interface{}, error) {
	var j map[string]interface{}
	if err := json.Unmarshal([]byte(body), &j); err != nil {
		return nil, err
	}
	return j[key], nil
}

type productAttributes struct {
	ResourceID  string
	ProductCode string
}

func getProductAttributes(ci models.ConfigItem) (productAttributes, error) {
	var resourceID, productCode string

	switch *ci.ExternalType {
	case v1.AWSEC2Instance:
		resourceID = *ci.Name
		productCode = "AmazonEC2"

	case v1.AWSEKSCluster:
		arn, err := getJSONKey(*ci.Config, "arn")
		if err != nil {
			return productAttributes{}, err
		}
		resourceID = arn.(string)
		productCode = "AmazonEKS"

	case v1.AWSS3Bucket:
		resourceID = *ci.Name
		productCode = "AmazonS3"

	case v1.AWSLoadBalancer:
		resourceID = fmt.Sprintf("arn:aws:elasticloadbalancing:%s:%s:loadbalancer/%s", *ci.Region, *ci.Account, *ci.Name)
		productCode = "AWSELB"

	case v1.AWSLoadBalancerV2:
		resourceID = ci.ExternalID[0]
		// TODO: Check
		productCode = "AWSELBV2"

	case v1.AWSEBSVolume:
		resourceID = *ci.Name
		productCode = "AmazonEC2"

	case v1.AWSRDSInstance:
		// TODO: Check
		resourceID = ci.ExternalID[0]
		productCode = "AmazonRDS"
	}

	return productAttributes{
		ResourceID:  resourceID,
		ProductCode: productCode,
	}, nil
}

func getAWSAthenaConfig(ctx *v1.ScrapeContext, awsConfig v1.AWS) (*athena.Config, error) {
	accessKey, secretKey, err := getAccessAndSecretKey(ctx, *awsConfig.AWSConnection)
	if err != nil {
		return nil, err
	}
	conf, err := athena.NewDefaultConfig(awsConfig.CostReporting.S3BucketPath, awsConfig.CostReporting.Region, accessKey, secretKey)
	return conf, err
}

type periodicCosts struct {
	Hourly  float64
	Daily   float64
	Weekly  float64
	Monthly float64
}

func FetchCosts(ctx *v1.ScrapeContext, config v1.AWS, ci models.ConfigItem) (periodicCosts, error) {
	attrs, err := getProductAttributes(ci)
	if err != nil {
		return periodicCosts{}, err
	}

	athenaConf, err := getAWSAthenaConfig(ctx, config)
	if err != nil {
		return periodicCosts{}, err
	}

	athenaDB, err := sql.Open(athena.DriverName, athenaConf.Stringify())
	if err != nil {
		return periodicCosts{}, err
	}

	table := fmt.Sprintf("%s.%s", config.CostReporting.Database, config.CostReporting.Table)
	query := strings.ReplaceAll(costQueryTemplate, "$table", table)
	queryArgs := []interface{}{sql.Named("id", attrs.ResourceID), sql.Named("product_code", attrs.ProductCode)}

	var costs periodicCosts
	if err = athenaDB.QueryRow(query, queryArgs...).Scan(&costs.Hourly, &costs.Daily, &costs.Weekly, &costs.Monthly); err != nil {
		return periodicCosts{}, nil
	}

	return costs, nil
}

type CostScraper struct{}

func (awsCost CostScraper) Scrape(ctx v1.ScrapeContext, config v1.ConfigScraper, _ v1.Manager) v1.ScrapeResults {
	var results v1.ScrapeResults

	for _, awsConfig := range config.AWS {
		session, err := NewSession(&ctx, *awsConfig.AWSConnection, awsConfig.Region[0])
		if err != nil {
			return results.Errorf(err, "failed to create AWS session")
		}
		STS := sts.NewFromConfig(*session)
		caller, err := STS.GetCallerIdentity(ctx, nil)
		if err != nil {
			return results.Errorf(err, "failed to get identity")
		}
		accountID := *caller.Account

		// fetch config items which match aws resources and account
		configItems, err := db.QueryAWSResources(accountID)
		if err != nil {
			return results.Errorf(err, "failed to query config items from db")
		}

		for _, configItem := range configItems {
			costs, err := FetchCosts(&ctx, awsConfig, configItem)
			if err != nil {
				// TODO Log error
				continue
			}
			results = append(results, v1.ScrapeResult{
				ID:            configItem.ID,
				CostPerMinute: costs.Hourly / 60,
				CostTotal1d:   costs.Daily,
				CostTotal7d:   costs.Weekly,
				CostTotal30d:  costs.Monthly,
			})
		}

	}

	return results
}
