// Copyright 2017 Yahoo Holdings, Inc.
// Licensed under the terms of the Apache version 2.0 license. See LICENSE file for terms.
// Copyright 2017 Yahoo Holdings, Inc.
// Licensed under the terms of the Apache version 2.0 license. See LICENSE file for terms.

package zpu

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ardielle/ardielle-go/rdl"
	"github.com/yahoo/athenz/clients/go/zms"
	"github.com/yahoo/athenz/clients/go/zts"
	"github.com/yahoo/athenz/libs/go/zmssvctoken"
	"github.com/yahoo/athenz/utils/zpe-updater/util"
)

func PolicyUpdater(config *ZpuConfiguration) error {
	if config == nil {
		return errors.New("Nil configuration")
	}
	if config.DomainList == "" {
		return errors.New("No domain list to process from configuration")
	}
	if config.Zms == "" {
		return errors.New("Empty Zms url in configuration")
	}
	if config.Zts == "" {
		return errors.New("Empty Zts url in configuration")
	}
	success := true
	domains := strings.Split(config.DomainList, ",")
	ztsUrl := formatUrl(config.Zts, "zts/v1")
	ztsClient := zts.NewClient(ztsUrl, nil)
	zmsUrl := formatUrl(config.Zms, "zms/v1")
	zmsClient := zms.NewClient(zmsUrl, nil)
	policyFileDir := config.PolicyFileDir
	failedDomains := ""
	for _, domain := range domains {
		err := GetPolicies(config, ztsClient, zmsClient, policyFileDir, domain)
		if err != nil {
			if success {
				success = false
			}
			failedDomains += `"`
			failedDomains += domain
			failedDomains += `" `
			log.Printf("Failed to get policies for domain: %v, Error:%v", domain, err)
		}
	}
	metricFilesPath := config.MetricsDir
	if metricFilesPath != "" {
		err := PostAllDomainMetric(ztsClient, metricFilesPath)
		if err != nil {
			log.Printf("Posting of metrics to Zts failed, Error:%v", err)
		}
	}
	if !success {
		return fmt.Errorf("Failed to get policies for domains: %v", failedDomains)
	}
	return nil
}

func GetPolicies(config *ZpuConfiguration, ztsClient zts.ZTSClient, zmsClient zms.ZMSClient, policyFileDir, domain string) error {
	log.Printf("Getting policies for domain: %v", domain)
	etag, err := GetEtagForExistingPolicy(config, zmsClient, domain, policyFileDir)
	if err != nil {
		return fmt.Errorf("Failed to get Etag for domain: %v, Error: %v", domain, err)
	}
	data, _, err := ztsClient.GetDomainSignedPolicyData(zts.DomainName(domain), etag)
	if err != nil {
		return fmt.Errorf("Failed to get domain signed policy data for domain: %v, Error:%v", domain, err)
	}

	if data == nil {
		if etag != "" {
			log.Printf("Policies not updated since last fetch for domain: %v", domain)
			return nil
		} else {
			return fmt.Errorf("Empty policies data returned for domain: %v", domain)
		}
	}
	//validate data using zts public key and signature
	err = ValidateSignedPolicies(config, zmsClient, data)
	if err != nil {
		return fmt.Errorf("Failed to validate policy data for domain: %v, Error: %v", domain, err)
	}
	err = WritePolicies(config, data, domain, policyFileDir)
	if err != nil {
		return fmt.Errorf("Unable to write Policies for domain:\"%v\" to file, Error:%v", domain, err)
	}
	log.Printf("Policies for domain: %v successfully written", domain)
	return nil
}

func GetEtagForExistingPolicy(config *ZpuConfiguration, zmsClient zms.ZMSClient, domain, policyFileDir string) (string, error) {
	var etag string
	var domainSignedPolicyData *zts.DomainSignedPolicyData

	policyFile := fmt.Sprintf("%s/%s.pol", policyFileDir, domain)

	// If Policies file is not found, return empty etag the first time
	// else load the file contents, if data has expired return empty etag, else construct etag from modified field in Json
	exists := util.Exists(policyFile)
	if !exists {
		return "", nil
	}

	readFile, err := os.OpenFile(policyFile, os.O_RDONLY, 0444)
	defer readFile.Close()
	if err != nil {
		return "", err
	}
	err = json.NewDecoder(readFile).Decode(&domainSignedPolicyData)
	if err != nil {
		return "", err
	}
	err = ValidateSignedPolicies(config, zmsClient, domainSignedPolicyData)
	if err != nil {
		return "", err
	}
	expires := domainSignedPolicyData.SignedPolicyData.Expires
	if expired(rdl.NewTimestamp(expires.Time.Add(time.Duration(int64(config.StartUpDelay)) * time.Second))) {
		return "", nil
	}
	modified := domainSignedPolicyData.SignedPolicyData.Modified
	if !modified.IsZero() {

		etag = "\"" + string(modified.String()) + "\""
	}
	return etag, nil
}

func ValidateSignedPolicies(config *ZpuConfiguration, zmsClient zms.ZMSClient, data *zts.DomainSignedPolicyData) error {
	expires := data.SignedPolicyData.Expires
	if expired(expires) {
		return fmt.Errorf("The policy data is expired on %v", expires)
	}
	signedPolicyData := data.SignedPolicyData
	ztsSignature := data.Signature
	ztsKeyId := data.KeyId

	ztsPublicKey := config.GetZtsPublicKey(ztsKeyId)
	if ztsPublicKey == "" {
		key, err := zmsClient.GetPublicKeyEntry("sys.auth", "zts", ztsKeyId)
		if err != nil {
			return fmt.Errorf("Unable to get the Zts public key with id:\"%v\" to verify data", ztsKeyId)
		}
		decodedKey, err := new(zmssvctoken.YBase64).DecodeString(key.Key)
		if err != nil {
			return fmt.Errorf("Unable to decode the Zts public key with id:\"%v\" to verify data", ztsKeyId)
		}
		ztsPublicKey = string(decodedKey)
	}
	input, err := util.ToCanonicalString(signedPolicyData)
	if err != nil {
		return err
	}
	err = verify(input, ztsSignature, ztsPublicKey)
	if err != nil {
		return fmt.Errorf("Verification of data with zts key having id:\"%v\" failed, Error :%v", ztsKeyId, err)
	}
	zmsSignature := data.SignedPolicyData.ZmsSignature
	zmsKeyId := data.SignedPolicyData.ZmsKeyId
	zmsPublicKey := config.GetZmsPublicKey(zmsKeyId)
	if zmsPublicKey == "" {
		key, err := zmsClient.GetPublicKeyEntry("sys.auth", "zms", zmsKeyId)
		if err != nil {
			return fmt.Errorf("Unable to get the Zms public key with id:\"%v\" to verify data", zmsKeyId)
		}
		decodedKey, err := new(zmssvctoken.YBase64).DecodeString(key.Key)
		if err != nil {
			return fmt.Errorf("Unable to decode the Zms public key with id:\"%v\" to verify data", zmsKeyId)
		}
		zmsPublicKey = string(decodedKey)
	}
	policyData := data.SignedPolicyData.PolicyData
	input, err = util.ToCanonicalString(policyData)
	if err != nil {
		return err
	}
	err = verify(input, zmsSignature, zmsPublicKey)
	if err != nil {
		return fmt.Errorf("Verification of data with zms key with id:\"%v\" failed, Error :%v", zmsKeyId, err)
	}
	return nil
}

func verify(input, signature, publicKey string) error {
	verifier, err := zmssvctoken.NewVerifier([]byte(publicKey))
	if err != nil {
		return err
	}
	err = verifier.Verify(input, signature)
	return err
}

func expired(expires rdl.Timestamp) bool {
	if rdl.TimestampNow().Millis() > expires.Millis() {
		return true
	} else {
		return false
	}
}

// If domain policy file is not found, create the policy file and write policies in it
// else delete the existing file and write the modified policies to new file
func WritePolicies(config *ZpuConfiguration, data *zts.DomainSignedPolicyData, domain, policyFileDir string) error {
	tempPolicyFileDir := config.TmpPolicyFileDir
	if tempPolicyFileDir == "" || data == nil {
		return errors.New("Empty parameters are not valid arguments")
	}
	policyFile := fmt.Sprintf("%s/%s.pol", policyFileDir, domain)
	tempPolicyFile := fmt.Sprintf("%s/%s.tmp", tempPolicyFileDir, domain)
	if util.Exists(tempPolicyFile) {
		err := os.Remove(tempPolicyFile)
		if err != nil {
			return err
		}
	}

	bytes, err := json.Marshal(&data)
	if err != nil {
		return err
	}
	err = verifyTmpDirSetup(tempPolicyFileDir)
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(tempPolicyFile, bytes, 0755)
	if err != nil {
		return err
	}
	os.Rename(tempPolicyFile, policyFile)
	if err != nil {
		return err
	}
	return nil
}

func verifyTmpDirSetup(TempPolicyFileDir string) error {
	if util.Exists(TempPolicyFileDir) {
		return nil
	}
	err := os.MkdirAll(TempPolicyFileDir, 0755)
	if err != nil {
		return err
	}
	return nil
}

func PostAllDomainMetric(ztsClient zts.ZTSClient, metricFilePath string) error {
	m, err := aggregateAllDomainMetrics(metricFilePath)
	if err != nil {
		return err
	}
	if m != nil {
		for key, value := range m {

			data, err := buildDomainMetrics(key, value)
			if err != nil {
				return err
			}
			log.Printf("Posting Domain metric for domain %v to Zts", key)
			data, err = ztsClient.PostDomainMetrics(zts.DomainName(key), data)
			if err != nil {
				log.Printf("Failed to post metrics for domain %v to Zts", key)
				return err
			}
			deleteDomainMetricFiles(metricFilePath, key)
		}
	}
	return nil
}

func aggregateAllDomainMetrics(metricFilePath string) (map[string]map[string]int, error) {
	var m = make(map[string]map[string]int)
	var fileMap = make(map[string]int)

	files, err := ioutil.ReadDir(metricFilePath)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, nil
	}
	for _, f := range files {
		data, err := ioutil.ReadFile(metricFilePath + "/" + f.Name())
		if err != nil {
			return nil, fmt.Errorf("Failed to read metric  file : %v, Error:%v", f.Name(), err)
		}
		fileMap = map[string]int{}
		err = json.Unmarshal(data, &fileMap)
		if err != nil {
			return nil, fmt.Errorf("Unmarshalling Error:%v for file : %v", err, f.Name())
		}
		domain := strings.Split(f.Name(), "_")
		if _, exists := m[domain[0]]; exists {
			domainMap := m[domain[0]]
			for key, value := range fileMap {
				if _, exists := domainMap[key]; exists {
					val := domainMap[key]
					val += value
					domainMap[key] = val
				} else {
					domainMap[key] = value
				}

			}
		} else {
			m[domain[0]] = fileMap
		}

	}
	return m, nil
}

func buildDomainMetrics(key string, value map[string]int) (*zts.DomainMetrics, error) {
	var data *zts.DomainMetrics
	counter := 1
	metricJson := `{"domainName":"` + key + `","metricList":[`
	valuekeys := []string{}
	for k, _ := range value {
		valuekeys = append(valuekeys, k)
	}
	sort.Strings(valuekeys)
	for _, innerKey := range valuekeys {
		size := len(value)
		if counter == size {
			metricJson += `{"metricType":"` + innerKey + `","metricVal":` + strconv.Itoa(value[innerKey]) + `}`
		} else {
			metricJson += `{"metricType":"` + innerKey + `","metricVal":` + strconv.Itoa(value[innerKey]) + `},`
		}
		counter += 1
	}
	metricJson += `]}`
	err := json.Unmarshal([]byte(metricJson), &data)
	if err != nil {
		return nil, err
	}
	return data, err
}

func deleteDomainMetricFiles(path, domainName string) {
	files, err := ioutil.ReadDir(path)
	if err != nil {
		log.Printf("Failed to get metric files at path for deletion: %v", path)
		return
	}
	for _, f := range files {
		domain := strings.Split(f.Name(), "_")
		if domain[0] == domainName {
			err := os.Remove(path + "/" + f.Name())
			if err != nil {
				log.Printf("Failed to delete file : % v for domain : %v", f.Name(), domainName)
			}
		}
	}
}

func formatUrl(url, suffix string) string {
	if !strings.HasSuffix(url, suffix) {
		if strings.LastIndex(url, "/") != len(url)-1 {
			url += "/"
		}
		url += suffix
	}
	return url
}
