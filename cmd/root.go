package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/hcl"
	"github.com/spf13/cobra"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/compute/v1"
)

type projectConfig struct {
	Project map[string]gceProject       `hcl:"project"`
	Ruleset map[string]rawConfigRuleset `hcl:"ruleset"`
}

type configRuleset struct {
	StartTime time.Time
	StopTime  time.Time
	Timezone  *time.Location
	Days      []int
	Instances []gceInstance

	rawRuleset rawConfigRuleset
}

type rawConfigRuleset struct {
	StartTime string `hcl:"startTime"`
	StopTime  string `hcl:"stopTime"`
	Timezone  string `hcl:"timezone"`
	Days      []int  `hcl:"days"`
}

type gceInstance struct {
	Name    string
	Project string
	Zone    string
	Status  string
}

type gceProject struct {
	Zones []string `hcl:"zones"`
}

func newRuleset(r rawConfigRuleset) (rs configRuleset, err error) {
	logPrintlnVerbose("Creating new ruleset")

	rs.rawRuleset = r

	timezone, locationErr := time.LoadLocation(r.Timezone)
	if locationErr != nil {
		err = multierror.Append(err, errors.New("timezone is not valid"))
	}

	rs.Timezone = timezone

	if r.StartTime == "" {
		err = multierror.Append(err, errors.New("startTime cannot be blank"))
	}

	if r.StopTime == "" {
		err = multierror.Append(err, errors.New("stopTime cannot be blank"))
	}

	if locationErr == nil {
		startTime, startTimeErr := time.ParseInLocation("15:04", r.StartTime, timezone)
		if startTimeErr != nil {
			err = multierror.Append(err, errors.New("startTime is not in valid 24 hour HH:mm format"))
		}

		rs.StartTime = startTime

		stopTime, stopTimeErr := time.ParseInLocation("15:04", r.StopTime, timezone)
		if stopTimeErr != nil {
			err = multierror.Append(err, errors.New("stopTime is not in valid 24 hour HH:mm format"))
		}

		rs.StopTime = stopTime
	}

	if len(r.Days) > 7 {
		err = multierror.Append(err, errors.New("days must be valid"))
	}

	for _, day := range r.Days {
		if day > 7 || day < 1 {
			err = multierror.Append(err, errors.New("days must be an int between 1 and 7"))
		}
	}
	rs.Days = r.Days

	return
}

// RootCmd is gce-sleep's root command.
// Every other command attached to RootCmd is a child command to it.
var RootCmd = &cobra.Command{
	Use:   "gce-sleep",
	Short: "gce-sleep is a tool for shutting down/starting up Google Cloud Engine instances based on tags for savings costs when not in use.",
	Run: func(cmd *cobra.Command, args []string) {
		logPrintlnVerbose("Starting gce-sleep")

		now := time.Now()

		logPrintlnVerbose("Basing time on:", now)

		configContent, err := ioutil.ReadFile(configLocation)
		if err != nil {
			log.Fatal(err)
		}

		var config projectConfig
		err = hcl.Unmarshal(configContent, &config)
		if err != nil {
			log.Fatal(err)
		}

		activeRules := make(map[string]configRuleset)
		for index, rawRuleset := range config.Ruleset {
			ruleset, err := newRuleset(rawRuleset)
			if err != nil {
				log.Fatal(err)
			} else {
				activeRules[index] = ruleset
			}
		}

		logPrintlnVerbose("Active rulesets:", activeRules)

		ctx := context.Background()

		client, err := google.DefaultClient(ctx, compute.CloudPlatformScope)
		if err != nil {
			log.Fatal(err)
		}

		computeService, err := compute.New(client)
		if err != nil {
			log.Fatal(err)
		}

		filter := fmt.Sprintf("labels.%s eq on", labelName)
		logPrintlnVerbose(fmt.Sprintf("Filter defined as: %q", filter))

		for projectName, project := range config.Project {
			logPrintlnVerbose(fmt.Sprintf("Checking project %q", projectName))

			for _, zoneName := range project.Zones {
				logPrintlnVerbose(fmt.Sprintf("Checking zone %q", zoneName))

				instancesReq := computeService.Instances.List(projectName, zoneName).Filter(filter)
				if err := instancesReq.Pages(ctx, func(page *compute.InstanceList) error {
					for _, instance := range page.Items {
						logPrintlnVerbose(fmt.Sprintf("Checking instance %q", instance.Name))
						for _, metadata := range instance.Metadata.Items {
							if metadata.Key == "gce-sleep-group" {
								logPrintlnVerbose(fmt.Sprintf("Instance %q qualifies", instance.Name))

								actionableInstances := activeRules[*metadata.Value]
								actionableInstances.Instances = append(actionableInstances.Instances, gceInstance{
									Project: projectName,
									Zone:    zoneName,
									Name:    instance.Name,
									Status:  instance.Status,
								})
								activeRules[*metadata.Value] = actionableInstances
							}
						}
					}

					return nil
				}); err != nil {
					log.Fatal(err)
				}
			}
		}

		for rulesetName, ruleset := range activeRules {
			nowTimezone := now.In(ruleset.Timezone)
			shouldBeRunning := shouldBeRunning(nowTimezone, ruleset.StartTime, ruleset.StopTime)

			logPrintlnVerbose(fmt.Sprintf("Total %d instances to be evaluated in ruleset %q", len(ruleset.Instances), rulesetName))

			for _, instance := range ruleset.Instances {
				logPrintlnVerbose(fmt.Sprintf("Evaluating instance %q", instance.Name))

				if shouldBeRunning && instance.Status == "TERMINATED" {
					logPrintlnVerbose(fmt.Sprintf("Instance %q currently stopped, starting", instance.Name))

					call := computeService.Instances.Start(instance.Project, instance.Zone, instance.Name)
					_, err := call.Do()
					if err != nil {
						log.Fatal(err)
					} else {
						log.Println(fmt.Sprintf("Instance %q starting", instance.Name))
					}
				} else if !shouldBeRunning && instance.Status == "RUNNING" {
					logPrintlnVerbose(fmt.Sprintf("Instance %q currently starting, stopped", instance.Name))

					call := computeService.Instances.Stop(instance.Project, instance.Zone, instance.Name)
					_, err := call.Do()
					if err != nil {
						log.Fatal(err)
					} else {
						log.Println(fmt.Sprintf("Instance %q stopping", instance.Name))
					}
				} else {
					logPrintlnVerbose(fmt.Sprintf("Instance %q does not meet criteria (%s <=, %s >=)", instance.Name, ruleset.rawRuleset.StartTime, ruleset.rawRuleset.StopTime))
				}
			}
		}

		logPrintlnVerbose("Stopping gce-sleep")
	},
}

func shouldBeRunning(now, startTime, stopTime time.Time) bool {
	if startTime.Hour() > now.Hour() || stopTime.Hour() < now.Hour() {
		return false
	}

	if startTime.Hour() == now.Hour() && startTime.Minute() > now.Minute() {
		return false
	}

	if stopTime.Hour() == now.Hour() && stopTime.Minute() < now.Minute() {
		return false
	}

	return true
}
