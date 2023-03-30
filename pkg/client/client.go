package client

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/DockerContainerService/image-save/pkg/tools"
	"github.com/containers/image/v5/docker"
	"github.com/containers/image/v5/manifest"
	"github.com/containers/image/v5/pkg/blobinfocache/none"
	"github.com/containers/image/v5/types"
	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"io"
	"strings"
)

type Client struct {
	sourceRef  types.ImageReference
	source     types.ImageSource
	ctx        context.Context
	sysContext *types.SystemContext

	repo *repoUrl
}

func NewClient(sourceUrl, username, password string, insecure bool) *Client {
	repo, err := parseRepoUrl(sourceUrl)
	if err != nil {
		logrus.Fatalf("parseRepoUrl error: %+v", err)
	}
	srcRef, err := docker.ParseReference(fmt.Sprintf("//%s/%s:%s", repo.registry, strings.Join([]string{repo.namespace, repo.project}, "/"), repo.tag))

	var sysContext *types.SystemContext
	if insecure {
		sysContext = &types.SystemContext{
			DockerInsecureSkipTLSVerify: types.OptionalBoolTrue,
		}
	} else {
		sysContext = &types.SystemContext{}
	}

	ctx := context.WithValue(context.Background(), ctxKey{"ImageSource"}, strings.Join([]string{repo.namespace, repo.project}, "/"))
	if username != "" && password != "" {
		sysContext.DockerAuthConfig = &types.DockerAuthConfig{
			Username: username,
			Password: password,
		}
	}

	source, err := srcRef.NewImageSource(ctx, sysContext)
	if err != nil {
		logrus.Fatalf("get image source error: %+v", err)
	}

	return &Client{
		sourceRef:  srcRef,
		source:     source,
		ctx:        ctx,
		sysContext: sysContext,
		repo:       repo,
	}
}

func (c *Client) manifestHandler(manifestBytes []byte, manifestType string, osFilterList, archFilterList []string, parent *manifest.Schema2List) ([]manifest.Manifest, interface{}, error) {
	var manifestInfoList []manifest.Manifest
	if manifestType == manifest.DockerV2Schema2MediaType {
		manifestInfo, err := manifest.Schema2FromManifest(manifestBytes)
		if err != nil {
			return nil, nil, err
		}

		if parent == nil && manifestInfo.ConfigInfo().Digest != "" {
			blob, _, err := c.source.GetBlob(c.ctx, types.BlobInfo{Digest: manifestInfo.ConfigInfo().Digest, URLs: manifestInfo.ConfigInfo().URLs, Size: manifestInfo.ConfigInfo().Size}, none.NoCache)
			if err != nil {
				return nil, nil, err
			}
			defer blob.Close()
			bytes, err := io.ReadAll(blob)
			if err != nil {
				return nil, nil, err
			}
			results := gjson.GetManyBytes(bytes, "architecture", "os")

			if !platformValidate(osFilterList, archFilterList, &manifest.Schema2PlatformSpec{
				Architecture: results[0].String(),
				OS:           results[1].String(),
			}) {
				return manifestInfoList, manifestInfo, nil
			}
		}

		manifestInfoList = append(manifestInfoList, manifestInfo)
		return manifestInfoList, nil, nil
	} else if manifestType == manifest.DockerV2Schema1MediaType || manifestType == manifest.DockerV2Schema1SignedMediaType {
		manifestInfo, err := manifest.Schema1FromManifest(manifestBytes)
		if err != nil {
			return nil, nil, err
		}
		if parent == nil && !platformValidate(osFilterList, archFilterList, &manifest.Schema2PlatformSpec{
			Architecture: manifestInfo.Architecture,
		}) {
			return manifestInfoList, manifestInfo, nil
		}
		manifestInfoList = append(manifestInfoList, manifestInfo)
		return manifestInfoList, nil, nil
	} else if manifestType == manifest.DockerV2ListMediaType {
		manifestSchemaListInfo, err := manifest.Schema2ListFromManifest(manifestBytes)
		if err != nil {
			return nil, nil, err
		}

		var nm []manifest.Schema2ManifestDescriptor

		for _, manifestDescriptorElem := range manifestSchemaListInfo.Manifests {
			if !platformValidate(osFilterList, archFilterList, &manifestDescriptorElem.Platform) {
				continue
			}

			nm = append(nm, manifestDescriptorElem)

			manifestByte, manifestType, err := c.source.GetManifest(c.ctx, &manifestDescriptorElem.Digest)
			if err != nil {
				return nil, nil, err
			}

			platformSpecManifest, _, err := c.manifestHandler(manifestByte, manifestType, osFilterList, archFilterList, manifestSchemaListInfo)
			if err != nil {
				return nil, nil, err
			}

			manifestInfoList = append(manifestInfoList, platformSpecManifest...)
		}

		if len(nm) != len(manifestSchemaListInfo.Manifests) {
			manifestSchemaListInfo.Manifests = nm
			return manifestInfoList, manifestSchemaListInfo, nil
		}

		return manifestInfoList, nil, nil
	}

	return nil, nil, fmt.Errorf("unsupported manifest type: %v", manifestType)
}

func (c *Client) Save(osFilterList, archFilterList []string, output string) {
	logrus.Infof("Using architecture: %s", strings.Join(archFilterList, ","))
	manifestBytes, manifestType, err := c.source.GetManifest(c.ctx, nil)
	if err != nil {
		logrus.Fatalf("get manifest error: %+v", err)
	}
	manifestInfoList, _, err := c.manifestHandler(manifestBytes, manifestType, osFilterList, archFilterList, nil)

	if len(manifestInfoList) == 0 {
		logrus.Fatalf("%s: mismatch of os[%s] or architecture[%s]", c.repo.url, strings.Join(osFilterList, ","), strings.Join(archFilterList, ","))
	} else if len(manifestInfoList) > 1 {
		logrus.Fatalf("%s: matched of os[%s] and architecture[%s] greater than 1", c.repo.url, strings.Join(osFilterList, ","), strings.Join(archFilterList, ","))
	}

	configInfo := manifestInfoList[0].ConfigInfo()
	blob, size, err := c.source.GetBlob(c.ctx, types.BlobInfo{Digest: configInfo.Digest, URLs: configInfo.URLs, Size: configInfo.Size}, none.NoCache)
	if err != nil {
		logrus.Fatalf("load config info error: %+v", err)
	}
	configRes, err := io.ReadAll(blob)
	if err != nil {
		logrus.Fatalf("load config blob error: %+v", err)
	}

	// 开始导出
	// 目录准备
	destDir := fmt.Sprintf("%s_%s", strings.Join([]string{c.repo.namespace, c.repo.project}, "_"), c.repo.tag)

	if output == "" {
		output = fmt.Sprintf("%s.tgz", destDir)
	}

	if tools.IsPathExist(destDir) {
		err = tools.RemovePath(destDir)
		if err != nil {
			logrus.Fatalf("target dir already exists: %s", destDir)
		}
	}
	tools.MkdirPath(destDir)

	// 开始写文件
	tools.WriteFile(fmt.Sprintf("%s/%s.json", destDir, configInfo.Digest[7:]), configRes)

	type manifestBody struct {
		Config   string   `json:"Config"`
		RepoTags []string `json:"RepoTags"`
		Layers   []string `json:"Layers"`
	}

	manifestJson := []manifestBody{
		{
			Config:   fmt.Sprintf("%s.json", configInfo.Digest[7:]),
			RepoTags: []string{fmt.Sprintf("%s", c.repo.url)},
			Layers:   make([]string, 0),
		},
	}

	if !strings.Contains(c.repo.url, ":") {
		manifestJson[0].RepoTags = []string{fmt.Sprintf("%s:%s", c.repo.url, c.repo.tag)}
	}

	emptyJson := `{"created":"1970-01-01T00:00:00Z","container_config":{"Hostname":"","Domainname":"","User":"","AttachStdin":false,
	"AttachStdout":false,"AttachStderr":false,"Tty":false,"OpenStdin":false, "StdinOnce":false,"Env":null,"Cmd":null,"Image":"",
	"Volumes":null,"WorkingDir":"","Entrypoint":null,"OnBuild":null,"Labels":null}}`

	parentId := ""
	var layerDirId string

	for index, layer := range manifestInfoList[0].LayerInfos() {
		layerDigest := layer.Digest
		logrus.Debugf("Digest: %s", layerDigest)
		layerDirId = fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s%s", parentId, layerDigest))))
		layerDir := fmt.Sprintf("%s/%s", destDir, layerDirId)
		tools.MkdirPath(layerDir)

		logrus.Debugf("create Version file")
		tools.WriteFile(fmt.Sprintf("%s/VERSION", layerDir), []byte("1.0"))

		logrus.Debugf("create layer.tar")
		blob, size, err = c.source.GetBlob(c.ctx, types.BlobInfo{Digest: layerDigest, URLs: layer.URLs, Size: layer.Size}, none.NoCache)
		tools.WriteBufferedFile(fmt.Sprintf("%s/layer.tar", layerDir), blob, size, fmt.Sprintf("[%d/%d] %s", index+1, len(manifestInfoList[0].LayerInfos()), string(layerDigest[7:19])))

		manifestJson[0].Layers = append(manifestJson[0].Layers, fmt.Sprintf("%s/layer.tar", layerDirId))

		logrus.Debugf("create json file")
		jsonObj := make(map[string]interface{})
		if manifestInfoList[0].LayerInfos()[len(manifestInfoList[0].LayerInfos())-1].Digest == layerDigest {
			err = json.Unmarshal(configRes, &jsonObj)
			if err != nil {
				logrus.Fatalf("create json file error-1: %+v", err)
			}
			delete(jsonObj, "history")
			delete(jsonObj, "rootfs")
		} else {
			err = json.Unmarshal([]byte(emptyJson), &jsonObj)
			if err != nil {
				logrus.Fatalf("create json file error-2: %+v", err)
			}
		}
		jsonObj["id"] = layerDirId
		if parentId != "" {
			jsonObj["parent"] = parentId
		}
		parentId = layerDirId
		jsonObjByte, err := json.Marshal(jsonObj)
		if err != nil {
			logrus.Fatalf("create json file error-3: %+v", err)
		}
		tools.WriteFile(fmt.Sprintf("%s/json", layerDir), jsonObjByte)
	}

	logrus.Debugf("create manifest.json")
	manifestByte, err := json.Marshal(manifestJson)
	if err != nil {
		logrus.Fatalf("marshal manifestJson error: %+v", err)
	}
	tools.WriteFile(fmt.Sprintf("%s/manifest.json", destDir), manifestByte)

	logrus.Debugf("create repositories file")
	repositoryInfo := fmt.Sprintf("{\"%s\":{\"%s\":\"%x\"}}", c.repo.url, c.repo.tag, layerDirId)
	tools.WriteFile(fmt.Sprintf("%s/repositories", destDir), []byte(repositoryInfo))

	logrus.Debugf("tar %s -> %s", destDir, output)
	tools.TarDir(destDir, output)

	logrus.Debugf("remove tmp dir")
	err = tools.RemovePath(destDir)
	if err != nil {
		logrus.Fatalf("remove %s error: %+v", destDir, err)
	}

	logrus.Infof("Output file: %s", output)
}
