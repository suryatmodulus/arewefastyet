/*
 *
 * Copyright 2021 The Vitess Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 * /
 */

package exec

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"

	"github.com/google/uuid"
	"github.com/spf13/viper"
	"github.com/vitessio/arewefastyet/go/exec/stats"
	"github.com/vitessio/arewefastyet/go/infra"
	"github.com/vitessio/arewefastyet/go/infra/ansible"
	"github.com/vitessio/arewefastyet/go/infra/construct"
	"github.com/vitessio/arewefastyet/go/infra/equinix"
	"github.com/vitessio/arewefastyet/go/slack"
	"github.com/vitessio/arewefastyet/go/storage/influxdb"
	"github.com/vitessio/arewefastyet/go/storage/mysql"
	"github.com/vitessio/arewefastyet/go/tools/git"
	"github.com/vitessio/arewefastyet/go/tools/macrobench"
	"github.com/vitessio/arewefastyet/go/tools/microbench"
)

const (
	// keyExecUUID is the name of the key passed to each Ansible playbook
	// the value of the key points to an Execution UUID.
	keyExecUUID = "arewefastyet_exec_uuid"

	// keyExecSource is the name of the key that stores the name of the
	// execution's trigger.
	keyExecSource = "arewefastyet_source"

	// keyVitessVersion is the name of the key that stores the git reference
	// or SHA on which benchmarks will be executed.
	keyVitessVersion = "vitess_git_version"

	stderrFile = "exec-stderr.log"
	stdoutFile = "exec-stdout.log"

	ErrorNotPrepared = "exec is not prepared"
)

type Exec struct {
	UUID          uuid.UUID
	InfraConfig   infra.Config
	AnsibleConfig ansible.Config
	Infra         infra.Infra
	Source        string
	GitRef        string

	// Defines the type of execution (oltp, tpcc, micro, ...)
	typeOf string

	// Configuration used to interact with the SQL database.
	configDB *mysql.ConfigDB

	// Client to communicate with the SQL database.
	clientDB *mysql.Client

	// Configuration used to authenticate and insert execution stats
	// data to a remote database system.
	statsRemoteDBConfig stats.RemoteDBConfig

	// Configuration used to send message to Slack.
	slackConfig slack.Config

	// rootDir represents the parent directory of the Exec.
	// From there, the Exec's unique directory named Exec.dirPath will
	// be created once Exec.Prepare is called.
	rootDir string

	// dirPath is Exec's unique directory where all reports, directories,
	// files, and logs are kept.
	dirPath string

	stdout io.Writer
	stderr io.Writer

	prepared   bool
	configPath string
}

// SetStdout sets the standard output of Exec.
func (e *Exec) SetStdout(stdout *os.File) {
	e.stdout = stdout
	e.AnsibleConfig.SetStdout(stdout)
}

// SetStderr sets the standard error output of Exec.
func (e *Exec) SetStderr(stderr *os.File) {
	e.stderr = stderr
	e.AnsibleConfig.SetStderr(stderr)
}

// SetOutputToDefaultPath sets Exec's outputs to their default files (stdoutFile and
// stderrFile). If they can't be found in Exec.dirPath, they will be created.
func (e *Exec) SetOutputToDefaultPath() error {
	if !e.prepared {
		return errors.New(ErrorNotPrepared)
	}
	outFile, err := os.OpenFile(path.Join(e.dirPath, stdoutFile), os.O_RDWR|os.O_CREATE, 0755)
	if err != nil {
		return err
	}

	errFile, err := os.OpenFile(path.Join(e.dirPath, stderrFile), os.O_RDWR|os.O_CREATE, 0755)
	if err != nil {
		return err
	}

	e.stdout = outFile
	e.stderr = errFile
	e.AnsibleConfig.SetOutputs(outFile, errFile)
	return nil
}

// Prepare prepares the Exec for a future Execution.
func (e *Exec) Prepare() error {
	// Returns if the execution is already prepared
	if e.prepared {
		return nil
	}

	var err error
	e.clientDB, err = mysql.New(*e.configDB)
	if err != nil {
		return err
	}

	// insert new exec in SQL
	if _, err = e.clientDB.Insert("INSERT INTO execution(uuid, status, source, git_ref, type) VALUES(?, ?, ?, ?, ?)", e.UUID.String(), StatusCreated, e.Source, e.GitRef, e.typeOf); err != nil {
		return err
	}

	e.Infra.SetTags(map[string]string{
		"execution_git_ref": git.ShortenSHA(e.GitRef),
		"execution_source":  e.Source,
	})

	err = e.prepareDirectories()
	if err != nil {
		return err
	}

	err = e.Infra.Prepare()
	if err != nil {
		return err
	}
	if e.configPath == "" {
		e.configPath = viper.ConfigFileUsed()
	}
	e.AnsibleConfig.ExtraVars = map[string]interface{}{}
	e.statsRemoteDBConfig.AddToAnsible(&e.AnsibleConfig)
	e.prepared = true
	return nil
}

// Execute will provision infra, configure Ansible files, and run the given Ansible config.
func (e *Exec) Execute() (err error) {
	defer func() {
		status := StatusFinished
		if err != nil {
			status = StatusFailed
		}
		_, _ = e.clientDB.Insert("UPDATE execution SET finished_at = CURRENT_TIME, status = ? WHERE uuid = ?", status, e.UUID.String())
	}()

	if !e.prepared {
		return errors.New(ErrorNotPrepared)
	}
	if _, err := e.clientDB.Insert("UPDATE execution SET started_at = CURRENT_TIME, status = ? WHERE uuid = ?", StatusStarted, e.UUID.String()); err != nil {
		return err
	}

	IPs, err := e.provision()
	if err != nil {
		return err
	}

	// TODO: optimize tokenization of Ansible files.
	err = ansible.AddIPsToFiles(IPs, e.AnsibleConfig)
	if err != nil {
		return err
	}
	err = ansible.AddLocalConfigPathToFiles(e.configPath, e.AnsibleConfig)
	if err != nil {
		return err
	}

	e.AnsibleConfig.ExtraVars[keyExecUUID] = e.UUID.String()
	e.AnsibleConfig.ExtraVars[keyVitessVersion] = e.GitRef
	e.AnsibleConfig.ExtraVars[keyExecSource] = e.Source

	// Infra will run the given config.
	err = e.Infra.Run(&e.AnsibleConfig)
	if err != nil {
		return err
	}
	return nil
}

func (e Exec) SendNotificationForRegression() error {
	previousExec, previousGitRef, err := e.getPreviousFromSameSource()
	if err != nil {
		return err
	}
	if previousExec == "" {
		return nil
	}

	header := `*Observed a regression.*
Comparing: recent commit <https://github.com/vitessio/vitess/commit/` + e.GitRef + `|` + git.ShortenSHA(e.GitRef) + `> with old commit <https://github.com/vitessio/vitess/commit/` + previousGitRef + `|` + git.ShortenSHA(previousGitRef) + `>.
Benchmark UUIDs, recent: ` + e.UUID.String()[:7] + ` old: ` + previousExec[:7] + `.


`

	if e.typeOf == "micro" {
		microBenchmarks, err := microbench.CompareMicroBenchmarks(e.clientDB, e.GitRef, previousGitRef)
		if err != nil {
			return err
		}
		regression := microBenchmarks.Regression()
		if regression != "" {
			err = e.sendSlackMessage(regression, header)
			if err != nil {
				return err
			}
		}
	} else if e.typeOf == "oltp" || e.typeOf == "tpcc" {
		influxCfg := influxdb.Config{
			Host:     e.statsRemoteDBConfig.Host,
			Port:     e.statsRemoteDBConfig.Port,
			User:     e.statsRemoteDBConfig.User,
			Password: e.statsRemoteDBConfig.Password,
			Database: e.statsRemoteDBConfig.DbName,
		}
		influxClient, err := influxCfg.NewClient()
		if err != nil {
			return err
		}

		macrosMatrices, err := macrobench.CompareMacroBenchmarks(e.clientDB, influxClient, e.GitRef, previousGitRef)
		if err != nil {
			return err
		}

		macroResults := macrosMatrices[macrobench.Type(e.typeOf)].(macrobench.ComparisonArray)
		if len(macroResults) == 0 {
			return fmt.Errorf("no macrobenchmark result")
		}

		regression := macroResults[0].Regression()
		if regression != "" {
			err = e.sendSlackMessage(regression, header)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (e Exec) sendSlackMessage(regression, header string) error {
	content := header + regression
	msg := slack.TextMessage{Content: content}
	err := msg.Send(e.slackConfig)
	if err != nil {
		return err
	}
	return nil
}

// CleanUp cleans and removes all things required only during the execution flow
// and not after it is done.
func (e Exec) CleanUp() error {
	err := e.Infra.CleanUp()
	if err != nil {
		return err
	}
	return nil
}

// NewExec creates a new *Exec with an autogenerated uuid.UUID as well
// as a constructed infra.Infra.
func NewExec() (*Exec, error) {
	// todo: dynamic choice for infra provider
	inf, err := construct.NewInfra(equinix.Name)
	if err != nil {
		return nil, err
	}

	ex := Exec{
		UUID:  uuid.New(),
		Infra: inf,

		// By default Exec prints os.Stdout and os.Stderr.
		// This can be changed later by explicitly using
		// exec.SetStdout and exec.SetStderr, or SetOutputToDefaultPath.
		stdout: os.Stdout,
		stderr: os.Stderr,

		configDB:   &mysql.ConfigDB{},
		clientDB:   nil,
		configPath: viper.ConfigFileUsed(),
	}

	// ex.AnsibleConfig.SetOutputs(ex.stdout, ex.stderr)
	ex.Infra.SetConfig(&ex.InfraConfig)
	ex.Infra.SetExecUUID(ex.UUID)

	return &ex, nil
}

// NewExecWithConfig will create a new Exec using the NewExec method, and will
// use viper.Viper to apply the configuration located at pathConfig.
func NewExecWithConfig(pathConfig string) (*Exec, error) {
	e, err := NewExec()
	if err != nil {
		return nil, err
	}
	v := viper.New()

	v.SetConfigFile(pathConfig)
	if err := v.ReadInConfig(); err != nil {
		return nil, err
	}

	err = e.AddToViper(v)
	if err != nil {
		return nil, err
	}
	e.configPath = pathConfig
	return e, nil
}

func (e Exec) getPreviousFromSameSource() (execUUID, gitRef string, err error) {
	query := "SELECT e.uuid, e.git_ref FROM execution e WHERE e.source = ? AND e.status = 'finished' AND " +
		"e.type = ? AND e.git_ref != ? ORDER BY e.started_at DESC LIMIT 1"
	result, err := e.clientDB.Select(query, e.Source, e.typeOf, e.GitRef)
	if err != nil {
		return
	}
	for result.Next() {
		err = result.Scan(&execUUID, &gitRef)
		if err != nil {
			return
		}
	}
	return
}

// GetLatestCronJobForMicrobenchmarks will fetch and return the commit sha for which
// the last cron job for microbenchmarks was run
func GetLatestCronJobForMicrobenchmarks(client *mysql.Client) (gitSha string, err error) {
	query := "select git_ref from execution where source = \"cron\" and status = \"finished\" order by started_at desc limit 1"
	rows, err := client.Select(query)
	if err != nil {
		return "", err
	}

	for rows.Next() {
		err = rows.Scan(&gitSha)
		return gitSha, err
	}
	return "", nil
}

func Exists(clientDB *mysql.Client, gitRef, source, typeOf, status string) (bool, error) {
	query := "SELECT uuid FROM execution WHERE status = ? AND git_ref = ? AND type = ? AND source = ?"
	result, err := clientDB.Select(query, status, gitRef, typeOf, source)
	if err != nil {
		return false, err
	}
	return result.Next(), nil
}
