package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"regexp"
	"strings"
	"sync"
	"text/template"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/google/go-github/v47/github"
	"github.com/spf13/pflag"
	"golang.org/x/oauth2"
	"gopkg.in/yaml.v3"
)

// RulesFile 读取规则文件路径
const RulesFile = "rules.yaml"

var (
	config    = &Config{}
	resultTpl = `
{{ if .Success }}
**转换完成**
^^^bash
{{ if .Registry }}
docker login -u{{ .RegistryUser }} {{ .Registry }}
{{ end }}
#原镜像
{{ .OriginImageName }}

#转换后镜像
{{ .TargetImageName }}


#下载并重命名镜像
docker pull {{ .TargetImageName }}

docker tag  {{ .TargetImageName }} {{ .OriginImageName }}

docker images | grep $(echo {{ .OriginImageName }} |awk -F':' '{print $1}')

^^^
{{ else }}
**转换失败**
详见 [构建任务](https://github.com/{{ .GhUser }}/{{ .Repo }}/actions/runs/{{ .RunId }})
{{ end }}
`
)

// Config 用来记录程序执行的配置信息
type Config struct {
	GhToken           string            `yaml:"gh_token"`
	GhUser            string            `yaml:"gh_user"`
	Repo              string            `yaml:"repo"`
	Registry          string            `yaml:"registry"`
	RegistryNamespace string            `yaml:"registry_namespace"`
	RegistryUserName  string            `yaml:"registry_user_name"`
	RegistryPassword  string            `yaml:"registry_password"`
	Rules             map[string]string `yaml:"rules"`
	RunId             string            `yaml:"run_id"`
	MaxCount          int               `yaml:"max_count"`
	RulesFile         string            `yaml:"rules_file"`
}

// Result 用来记录执行结果
type Result struct {
	Success         bool
	Registry        string
	RegistryUser    string
	OriginImageName string
	TargetImageName string
	GhUser          string
	Repo            string
	RunId           string
}

func init() {
	pflag.CommandLine.StringVarP(&config.GhToken, "github.token", "t", "", "Github token.")
	pflag.CommandLine.StringVarP(&config.GhUser, "github.user", "u", "", "Github Owner.")
	pflag.CommandLine.StringVarP(&config.Repo, "github.repo", "p", "", "Github Repo.")
	pflag.CommandLine.StringVarP(&config.Registry, "docker.registry", "r", "", "Docker Registry.")
	pflag.CommandLine.StringVarP(&config.RegistryNamespace, "docker.namespace", "n", "", "Docker Registry Namespace.")
	pflag.CommandLine.StringVarP(&config.RegistryUserName, "docker.user", "a", "", "Docker Registry User.")
	pflag.CommandLine.StringVarP(&config.RegistryPassword, "docker.secret", "s", "", "Docker Registry Password.")
	pflag.CommandLine.StringVarP(&config.RunId, "github.run_id", "i", "", "Github Run Id.")
	pflag.CommandLine.IntVarP(&config.MaxCount, "github.max_count", "m", 1, "max count issue process for one time.")
	pflag.CommandLine.StringVarP(&config.RulesFile, "rules.file", "c", RulesFile, "rules mapping file")
}

func main() {
	// 解析参数
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	pflag.Parse()

	// 给一个默认的映射，key=>仓库前缀 value=>推动到docker hub的repository
	config.Rules = map[string]string{
		"^gcr.io":          "",
		"^docker.io":       "docker",
		"^k8s.gcr.io":      "google-containers",
		"^registry.k8s.io": "google-containers",
		"^quay.io":         "quay",
		"^ghcr.io":         "ghcr",
	}

	// 从外部文件读取映射关系
	if rulesFile, err := ioutil.ReadFile(RulesFile); err == nil {
		rules := make(map[string]string)
		if err := yaml.Unmarshal(rulesFile, &rules); err == nil {
			config.Rules = rules
		}
	}
	// 定义一个全局使用的ctx
	ctx := context.Background()

	// 初始化github认证客户端
	githubCli := github.NewClient(oauth2.NewClient(ctx, oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: config.GhToken},
	)))
	// 获取Issue列表
	issues, _, err := githubCli.Issues.ListByRepo(ctx, config.GhUser, config.Repo, &github.IssueListByRepoOptions{
		State:     "open",
		Labels:    []string{"porter"},
		Sort:      "created",
		Direction: "desc",
		// 每次处理的issue数量，暂时只配置一个
		ListOptions: github.ListOptions{Page: 1, PerPage: config.MaxCount},
	})
	if err != nil {
		fmt.Println("获取Issues列表报错", err.Error())
		os.Exit(-1)
	}
	if len(issues) == 0 {
		fmt.Println("暂无需要搬运的镜像")
		os.Exit(0)
	}

	// 协程处理
	wg := sync.WaitGroup{}
	for i := range issues {
		wg.Add(1)
		go func(issue *github.Issue) {
			defer wg.Done()
			fmt.Println("添加 构建进展 Comment")
			if err := commentIssues(issue, githubCli, ctx, "[构建进展](https://github.com/"+config.GhUser+"/"+config.Repo+"/actions/runs/"+config.RunId+")"); err != nil {
				fmt.Println("提交 添加 构建进展 Comment 报错", err)
			}
			err, originImageName, targetImageName := mirrorByIssues(issue, config)
			if err != nil {
				commentErr := commentIssues(issue, githubCli, ctx, err.Error())
				if commentErr != nil {
					fmt.Println("提交 Main Process 报错", commentErr)
				}
			}
			// 将执行结果写入到Issue中
			result := Result{
				Success:         err == nil,
				Registry:        config.Registry,
				RegistryUser:    config.RegistryUserName,
				OriginImageName: originImageName,
				TargetImageName: targetImageName,
				GhUser:          config.GhUser,
				Repo:            config.Repo,
				RunId:           config.RunId,
			}
			var buf bytes.Buffer
			tmpl, err := template.New("result").Parse(resultTpl)
			if err != nil {
				fmt.Println("解析 内置tpl 报错", err)
				return
			}
			if err = tmpl.Execute(&buf, &result); err != nil {
				fmt.Println("执行 内置tpl 报错", err)
				return
			}

			fmt.Println("添加 转换结果 Comment")
			if err := commentIssues(issue, githubCli, ctx, strings.ReplaceAll(buf.String(), "^", "`")); err != nil {
				fmt.Println("提交 添加 转换结果 Comment 报错", err)
			}

			fmt.Println("添加 转换结果 Label")
			issuesAddLabels(issue, githubCli, ctx, result.Success)

			fmt.Println("关闭 Issues")
			issuesClose(issue, githubCli, ctx)
		}(issues[i])
	}
	wg.Wait()
}

func issuesClose(issues *github.Issue, cli *github.Client, ctx context.Context) {
	names := strings.Split(*issues.RepositoryURL, "/")
	state := "closed"
	_, _, _ = cli.Issues.Edit(ctx, names[len(names)-2], names[len(names)-1], issues.GetNumber(), &github.IssueRequest{
		State: &state,
	})
}
func issuesAddLabels(issues *github.Issue, cli *github.Client, ctx context.Context, success bool) {
	names := strings.Split(*issues.RepositoryURL, "/")

	label := "success"
	if !success {
		label = "failed"
	}
	_, _, _ = cli.Issues.AddLabelsToIssue(ctx, names[len(names)-2], names[len(names)-1], issues.GetNumber(), []string{label})
}
func commentIssues(issues *github.Issue, cli *github.Client, ctx context.Context, comment string) error {
	names := strings.Split(*issues.RepositoryURL, "/")
	_, _, err := cli.Issues.CreateComment(ctx, names[len(names)-2], names[len(names)-1], issues.GetNumber(), &github.IssueComment{
		Body: &comment,
	})
	return err
}

func mirrorByIssues(issues *github.Issue, config *Config) (err error, originImageName string, targetImageName string) {
	// 去掉前缀 [PORTER] 整体去除前后空格
	originImageName = strings.TrimSpace(strings.Replace(*issues.Title, "[PORTER]", "", 1))
	targetImageName = originImageName

	if strings.ContainsAny(originImageName, "@") {
		return errors.New("@" + *issues.GetUser().Login + " 不支持同步带摘要信息的镜像"), originImageName, targetImageName
	}

	registries := make([]string, 0)
	for k, v := range config.Rules {
		targetImageName = regexp.MustCompile(k).ReplaceAllString(targetImageName, v)
		registries = append(registries, k)
	}

	if strings.EqualFold(targetImageName, originImageName) {
		return errors.New("@" + *issues.GetUser().Login + " 暂不支持同步" + originImageName + ",目前仅支持同步 `" + strings.Join(registries, " ,") + "`镜像"), originImageName, targetImageName
	}

	targetImageName = strings.ReplaceAll(targetImageName, "/", ".")

	if len(config.RegistryNamespace) > 0 {
		targetImageName = config.RegistryNamespace + "/" + targetImageName
	}
	if len(config.Registry) > 0 {
		targetImageName = config.Registry + "/" + targetImageName
	}
	fmt.Println("source:", originImageName, " , target:", targetImageName)

	//execCmd("docker", "login", config.Registry, "-u", config.RegistryUserName, "-p", config.RegistryPassword)
	cli, ctx, err := dockerLogin(config)
	if err != nil {
		return errors.New("@" + config.GhUser + " ,docker login 报错 `" + err.Error() + "`"), originImageName, targetImageName
	}

	//execCmd("docker", "pull", originImageName)
	if err = dockerPull(originImageName, cli, ctx); err != nil {
		return errors.New("@" + *issues.GetUser().Login + " ,docker pull 报错 `" + err.Error() + "`"), originImageName, targetImageName
	}

	//execCmd("docker", "tag", originImageName, targetImageName)
	if err = dockerTag(originImageName, targetImageName, cli, ctx); err != nil {
		return errors.New("@" + *issues.GetUser().Login + " ,docker tag 报错 `" + err.Error() + "`"), originImageName, targetImageName
	}

	//execCmd("docker", "push", targetImageName)
	if err = dockerPush(targetImageName, cli, ctx, config); err != nil {
		return errors.New("@" + *issues.GetUser().Login + " ,docker push 报错 `" + err.Error() + "`"), originImageName, targetImageName
	}

	return nil, originImageName, targetImageName
}

func dockerLogin(config *Config) (*client.Client, context.Context, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, nil, err
	}
	fmt.Println("docker login, server: ", config.Registry, " user: ", config.RegistryUserName, ", password: ***")
	authConfig := types.AuthConfig{
		Username:      config.RegistryUserName,
		Password:      config.RegistryPassword,
		ServerAddress: config.Registry,
	}
	ctx := context.Background()
	_, err = cli.RegistryLogin(ctx, authConfig)
	if err != nil {
		return nil, nil, err
	}
	return cli, ctx, nil
}
func dockerPull(originImageName string, cli *client.Client, ctx context.Context) error {
	fmt.Println("docker pull ", originImageName)
	pullOut, err := cli.ImagePull(ctx, originImageName, types.ImagePullOptions{})
	if err != nil {
		return err
	}
	defer func() {
		_ = pullOut.Close()
	}()
	_, _ = io.Copy(os.Stdout, pullOut)
	return nil
}
func dockerTag(originImageName string, targetImageName string, cli *client.Client, ctx context.Context) error {
	fmt.Println("docker tag ", originImageName, " ", targetImageName)
	err := cli.ImageTag(ctx, originImageName, targetImageName)
	return err
}
func dockerPush(targetImageName string, cli *client.Client, ctx context.Context, config *Config) error {
	fmt.Println("docker push ", targetImageName)
	authConfig := types.AuthConfig{
		Username: config.RegistryUserName,
		Password: config.RegistryPassword,
	}
	if len(config.Registry) > 0 {
		authConfig.ServerAddress = config.Registry
	}
	encodedJSON, err := json.Marshal(authConfig)
	if err != nil {
		return err
	}
	authStr := base64.URLEncoding.EncodeToString(encodedJSON)

	pushOut, err := cli.ImagePush(ctx, targetImageName, types.ImagePushOptions{
		RegistryAuth: authStr,
	})
	if err != nil {
		return err
	}
	defer func() {
		_ = pushOut.Close()
	}()
	_, _ = io.Copy(os.Stdout, pushOut)
	return nil
}
