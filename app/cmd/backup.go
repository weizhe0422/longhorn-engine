package cmd

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"

	"github.com/longhorn/backupstore/cmd"

	replicaClient "github.com/longhorn/longhorn-engine/pkg/replica/client"
	"github.com/longhorn/longhorn-engine/pkg/sync"
	"github.com/longhorn/longhorn-engine/pkg/types"
	"github.com/longhorn/longhorn-engine/pkg/util"
)

func BackupCmd() cli.Command {
	return cli.Command{
		Name:      "backups",
		ShortName: "backup",
		Subcommands: []cli.Command{
			BackupCreateCmd(),
			BackupStatusCmd(),
			BackupRestoreCmd(),
			RestoreToFileCmd(),
			RestoreStatusCmd(),
			cmd.BackupRemoveCmd(),
			cmd.BackupListCmd(),
			cmd.InspectVolumeCmd(),
			cmd.InspectBackupCmd(),
			cmd.GetConfigMetadataCmd(),
		},
	}
}

func BackupCreateCmd() cli.Command {
	return cli.Command{
		Name:  "create",
		Usage: "create a backup in objectstore: create <snapshot> --dest <dest>",
		Flags: []cli.Flag{
			cli.StringFlag{
				Name:  "dest",
				Usage: "destination of backup if driver supports, would be url like s3://bucket@region/path/ or vfs:///path/",
			},
			cli.StringFlag{
				Name:  "backing-image-name",
				Usage: "specify backing image name of the volume for backup",
			},
			cli.StringFlag{
				Name:  "backing-image-checksum",
				Usage: "specify checksum of backing image file",
			},
			cli.StringSliceFlag{
				Name:  "label",
				Usage: "specify labels for backup, in the format of `--label key1=value1 --label key2=value2`",
			},
			cli.StringFlag{
				Name:  "backup-name",
				Usage: "specify the backup name. If it is not set, a random name will be generated automatically",
			},
		},
		Action: func(c *cli.Context) {
			if err := createBackup(c); err != nil {
				logrus.WithError(err).Fatalf("Error running create backup command")
			}
		},
	}
}

func BackupStatusCmd() cli.Command {
	return cli.Command{
		Name:  "status",
		Usage: "query the progress of the backup: status <backupID>",
		Flags: []cli.Flag{
			cli.StringFlag{
				Name:  "replica",
				Usage: "specify the replica address",
			},
		},
		Action: func(c *cli.Context) {
			if err := checkBackupStatus(c); err != nil {
				logrus.Fatalf("Error querying backup status: %v", err)
			}
		},
	}
}

func getReplicaModeMap(replicas []*types.ControllerReplicaInfo) map[string]types.Mode {
	replicaModeMap := make(map[string]types.Mode)
	for _, replica := range replicas {
		replicaModeMap[replica.Address] = replica.Mode
	}

	return replicaModeMap
}

func checkBackupStatus(c *cli.Context) error {
	backupID := c.Args().First()
	if backupID == "" {
		return fmt.Errorf("missing required parameter backupID")
	}

	controllerClient, err := getControllerClient(c)
	if err != nil {
		return err
	}
	defer controllerClient.Close()

	replicas, err := controllerClient.ReplicaList()
	if err != nil {
		return errors.Wrap(err, "failed to get replica list")
	}

	replicaAddress := c.String("replica")
	if replicaAddress == "" {
		// find a replica which has the corresponding backup
		for _, replica := range replicas {
			if replica.Mode != types.RW {
				continue
			}

			repClient, err := replicaClient.NewReplicaClient(replica.Address)
			if err != nil {
				logrus.WithError(err).Errorf("Cannot create a replica client for IP[%v]", replicaAddress)
				return err
			}

			_, err = sync.FetchBackupStatus(repClient, backupID, replica.Address)
			repClient.Close()
			if err == nil {
				replicaAddress = replica.Address
				break
			}
		}
	}
	if replicaAddress == "" {
		return fmt.Errorf("cannot find a replica which has the corresponding backup %s", backupID)
	}

	replicaModeMap := getReplicaModeMap(replicas)
	if mode := replicaModeMap[replicaAddress]; mode != types.RW {
		return fmt.Errorf("failed to get backup status on %s for %v: %v",
			replicaAddress, backupID, "unknown replica")
	}

	repClient, err := replicaClient.NewReplicaClient(replicaAddress)
	if err != nil {
		logrus.WithError(err).Errorf("Cannot create a replica client for IP[%v]", replicaAddress)
		return err
	}
	defer repClient.Close()

	status, err := sync.FetchBackupStatus(repClient, backupID, replicaAddress)
	if err != nil {
		return err
	}

	backupStatus, err := json.Marshal(status)
	if err != nil {
		return err
	}
	fmt.Println(string(backupStatus))
	return nil
}

func BackupRestoreCmd() cli.Command {
	return cli.Command{
		Name:  "restore",
		Usage: "restore a backup to current volume: restore <backup>",
		Action: func(c *cli.Context) {
			if err := restoreBackup(c); err != nil {
				errInfo, jsonErr := json.MarshalIndent(err, "", "\t")
				if jsonErr != nil {
					logrus.WithError(jsonErr).Errorf("Cannot marshal err [%v] to json", err)
				} else {
					// If the error is not `TaskError`, the marshaled result is an empty json string.
					if string(errInfo) != "{}" {
						fmt.Println(string(errInfo))
					} else {
						fmt.Println(err.Error())
					}
				}
				logrus.WithError(err).Fatalf("Error running restore backup command")
			}
		},
	}
}

func RestoreStatusCmd() cli.Command {
	return cli.Command{
		Name:  "restore-status",
		Usage: "Check if restore operation is currently going on",

		Action: func(c *cli.Context) {
			if err := restoreStatus(c); err != nil {
				logrus.WithError(err).Fatalf("Error running restore backup command")
			}
		},
	}
}

func createBackup(c *cli.Context) error {
	dest := c.String("dest")
	if dest == "" {
		return fmt.Errorf("missing required parameter --dest")
	}

	snapshot := c.Args().First()
	if snapshot == "" {
		return fmt.Errorf("missing required parameter snapshot")
	}

	biName := c.String("backing-image-name")
	biChecksum := c.String("backing-image-checksum")

	labels := c.StringSlice("label")
	if labels != nil {
		// Only validate it here, the real parse is done at backend
		if _, err := util.ParseLabels(labels); err != nil {
			return errors.Wrap(err, "cannot parse backup labels")
		}
	}

	backupName := c.String("backup-name")

	credential, err := util.GetBackupCredential(dest)
	if err != nil {
		return err
	}

	url := c.GlobalString("url")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	task, err := sync.NewTask(ctx, url)
	if err != nil {
		return err
	}

	backup, err := task.CreateBackup(backupName, snapshot, dest, biName, biChecksum, labels, credential)
	if err != nil {
		return err
	}
	backupCreateInfo, err := json.Marshal(backup)
	if err != nil {
		return err
	}
	fmt.Println(string(backupCreateInfo))

	return nil
}

func restoreBackup(c *cli.Context) error {
	url := c.GlobalString("url")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	task, err := sync.NewTask(ctx, url)
	if err != nil {
		return err
	}

	backup := c.Args().First()
	if backup == "" {
		return fmt.Errorf("missing required parameter backup")
	}
	backupURL := util.UnescapeURL(backup)

	credential, err := util.GetBackupCredential(backup)
	if err != nil {
		return err
	}

	if err := task.RestoreBackup(backupURL, credential); err != nil {
		return err
	}

	return nil
}

func restoreStatus(c *cli.Context) error {
	url := c.GlobalString("url")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	task, err := sync.NewTask(ctx, url)
	if err != nil {
		return err
	}

	rsMap, err := task.RestoreStatus()
	if err != nil {
		return err
	}

	restoreStatus, err := json.Marshal(rsMap)
	if err != nil {
		return err
	}
	fmt.Println(string(restoreStatus))

	return nil
}
